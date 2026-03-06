#!/usr/bin/env python3
# ~/.context/ctx-detect.py
# Pattern detection across summarized traces. Finds recurring skills, co-occurring
# slugs, repeated pitfalls, and high-signal traces. Synthesizes candidate definitions
# via model_router.call(task="detect").
#
# Trigger: every 20 new summarized traces (counter in meta), or manual.
#
# Usage:
#   python3 ctx-detect.py                # run detection
#   python3 ctx-detect.py --dry-run      # show what would be detected, no AI calls
#   python3 ctx-detect.py --force        # ignore the 20-trace counter gate

import json, sqlite3, sys, re, hashlib
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))
from model_router import call

CONTEXT_DIR   = Path.home() / ".context"
CAND_DIR      = CONTEXT_DIR / "candidates"
DB_PATH       = CONTEXT_DIR / "index.db"

try:
    import toml
except ImportError:
    import tomllib as toml

def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")

# ── Synthesis prompt ───────────────────────────────────────────────────────────

DETECTION_SYSTEM_PROMPT = """You are analyzing a developer's recurring practices across multiple AI coding sessions.

Your job is to identify generalizable, actionable knowledge — not just label what happened.

You will be given multiple trace summaries that share a concept. Synthesize them into a single candidate insight.

NAMESPACE DEFINITIONS — pick exactly one:
- skills/internal: Repeatable procedural knowledge. "How to do X correctly." Must be actionable steps, not observations.
- patterns: Architectural decisions with tradeoffs. "When to choose X over Y and why." Must articulate the tradeoff.
- debugging: Failure mode + proven resolution. "X breaks when Y; fix is Z." Must name the specific failure and fix.
- primitives: Isolated, reusable code artifact with minimal context dependency. Rare — only for concrete templates or algorithms.

QUALITY BAR — a candidate is worth promoting only if it is:
- Generalizable (not specific to one project or task)
- Actionable (changes future decisions or saves debugging time)
- Non-obvious (not just "use the standard library" or "read the docs")

BODY FRAMING — write as forward-looking guidance, not historical summary.
Bad:  "Ben used tiered context injection in the chess coach chatbot to manage memory across sessions"
Good: "When building multi-session AI features, use three-layer context injection: static user profile, rolling conversation window, and task-specific state. Flatten to a single prompt block at injection time."

Bad:  "Vector search was used multiple times across projects"
Good: "When paginating over embedding results, pre-filter by metadata before computing cosine similarity. The n+1 embedding lookup pattern emerges when you fetch vectors inside a loop — batch-fetch IDs first, then load vectors in one call."

Respond with ONLY valid JSON. If no generalizable pattern exists (coincidental co-occurrence, too project-specific, or trivially obvious), return {"namespace": null}.

Otherwise return:
{
  "namespace": "skills/internal" | "patterns" | "debugging" | "primitives",
  "slug": "lowercase-hyphenated-2-4-words",
  "title": "Short descriptive title",
  "signal_strength": 1-5,
  "body": "3-6 sentences of forward-looking guidance. No project names. No proper nouns."
}"""


def build_synthesis_prompt(slug: str, trace_summaries: list) -> str:
    """Build the prompt for a single slug's synthesis."""
    parts = [f"You are analyzing {len(trace_summaries)} development traces that share the concept [{slug}].\n\nTraces:"]
    for i, ts in enumerate(trace_summaries, 1):
        parts.append(f"\n--- Trace {i} ({ts['trace_id']}) ---")
        parts.append(f"Problem: {ts['problem']}")
        parts.append(f"Pattern: {ts['pattern']}")
        parts.append(f"Skills: {ts['skills']}")
        parts.append(f"Pitfalls: {ts['pitfalls']}")
        parts.append(f"Summary: {ts['embedding_text']}")
    return "\n".join(parts)


def parse_json_response(raw: str) -> dict:
    """Parse JSON from model response. Strips markdown fences if present."""
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        pass
    stripped = re.sub(r'^```(?:json)?\s*', '', raw.strip())
    stripped = re.sub(r'\s*```$', '', stripped)
    return json.loads(stripped)


# ── Discovery ──────────────────────────────────────────────────────────────────

def get_slug_frequencies(conn: sqlite3.Connection) -> dict:
    """Return {slug: [list of trace summary dicts]} for all slugs."""
    rows = conn.execute(
        "SELECT trace_id, problem, pattern, skills, pitfalls, embedding_text "
        "FROM summaries WHERE skills IS NOT NULL"
    ).fetchall()

    slug_map = {}
    for row in rows:
        trace_id, problem, pattern, skills_json, pitfalls, embedding_text = row
        try:
            skills = json.loads(skills_json)
        except (json.JSONDecodeError, TypeError):
            continue
        if not isinstance(skills, list):
            skills = [skills]
        summary = {
            "trace_id": trace_id,
            "problem": problem or "",
            "pattern": pattern or "",
            "skills": skills_json,
            "pitfalls": pitfalls or "",
            "embedding_text": embedding_text or "",
        }
        for slug in skills:
            slug = slug.strip().lower()
            if slug:
                slug_map.setdefault(slug, []).append(summary)
    return slug_map


def get_pitfall_frequencies(conn: sqlite3.Connection) -> dict:
    """Return {pitfall_key: [trace summaries]} for repeated pitfalls."""
    rows = conn.execute(
        "SELECT trace_id, problem, pattern, skills, pitfalls, embedding_text "
        "FROM summaries WHERE pitfalls IS NOT NULL AND pitfalls != ''"
    ).fetchall()

    # Group pitfalls by their first 50 chars (rough dedup)
    pitfall_map = {}
    for row in rows:
        trace_id, problem, pattern, skills_json, pitfalls, embedding_text = row
        key = pitfalls.strip().lower()[:50]
        if key:
            summary = {
                "trace_id": trace_id,
                "problem": problem or "",
                "pattern": pattern or "",
                "skills": skills_json or "[]",
                "pitfalls": pitfalls,
                "embedding_text": embedding_text or "",
            }
            pitfall_map.setdefault(key, []).append(summary)
    return pitfall_map


def get_existing_candidates(conn: sqlite3.Connection) -> set:
    """Return set of slugs that already have a pending or promoted candidate."""
    rows = conn.execute(
        "SELECT slug FROM candidates WHERE status IN ('pending', 'approved')"
    ).fetchall()
    return {r[0] for r in rows}


def get_dismissed_candidates(conn: sqlite3.Connection) -> dict:
    """Return {slug: (dismissed_at, traces_since)} for dismissed candidates."""
    rows = conn.execute(
        "SELECT slug, resolved_at FROM candidates WHERE status = 'dismissed'"
    ).fetchall()
    result = {}
    for slug, resolved_at in rows:
        # Count traces summarized since dismissal
        count = conn.execute(
            "SELECT COUNT(*) FROM summaries WHERE summarized_at > ?",
            (resolved_at,)
        ).fetchone()[0]
        result[slug] = {"dismissed_at": resolved_at, "traces_since": count}
    return result


# ── Candidate writing ─────────────────────────────────────────────────────────

def write_candidate(conn: sqlite3.Connection, result: dict, source_traces: list,
                    frequency: int) -> str:
    """Write candidate to candidates/*.md and upsert to DB. Returns candidate ID."""
    namespace = result["namespace"]
    slug = result["slug"]
    title = result["title"]
    body = result["body"]
    signal = result.get("signal_strength", 3)
    trace_ids = [t["trace_id"] for t in source_traces]

    # Stable candidate ID
    cand_id = f"candidate-{namespace.replace('/', '-')}-{slug}"

    now = datetime.now(timezone.utc).isoformat()

    # Determine candidate type from namespace
    type_map = {
        "skills/internal": "skill",
        "patterns": "pattern",
        "debugging": "debugging",
        "primitives": "primitive",
    }
    cand_type = type_map.get(namespace, "pattern")

    # Write markdown
    CAND_DIR.mkdir(parents=True, exist_ok=True)
    md_path = CAND_DIR / f"candidate-{cand_type}-{slug}.md"
    frontmatter = f"""---
id: {cand_id}
type: {cand_type}
status: pending
detected_at: {now}
frequency: {frequency}
signal_strength: {signal}
source_traces: [{", ".join(trace_ids)}]
target_namespace: {namespace}
---
"""
    md_content = f"{frontmatter}\n# {title}\n\n{body}\n"
    md_path.write_text(md_content)

    # Upsert to DB
    conn.execute(
        """
        INSERT INTO candidates (id, type, slug, definition, status, frequency,
                                source_traces, detected_at)
        VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            definition=excluded.definition, frequency=excluded.frequency,
            source_traces=excluded.source_traces, detected_at=excluded.detected_at,
            status='pending'
        """,
        (cand_id, cand_type, slug, body, frequency, json.dumps(trace_ids), now),
    )
    conn.commit()

    print(f"  candidate: {cand_id} → {md_path.name}")
    return cand_id


# ── Main detection logic ───────────────────────────────────────────────────────

def run_detection(conn: sqlite3.Connection, dry_run: bool = False) -> int:
    """Run detection. Returns number of candidates created."""
    cfg = _config()
    min_freq = cfg["detection"]["min_freq"]

    existing = get_existing_candidates(conn)
    dismissed = get_dismissed_candidates(conn)
    slug_freq = get_slug_frequencies(conn)
    pitfall_freq = get_pitfall_frequencies(conn)

    # Collect candidates to synthesize
    to_synthesize = []

    # 1. Skill slugs appearing min_freq+ times
    for slug, traces in slug_freq.items():
        if len(traces) < min_freq:
            continue
        if slug in existing:
            continue
        # Re-surface dismissed candidates after 10 new traces
        if slug in dismissed:
            if dismissed[slug]["traces_since"] < 10:
                continue
            print(f"  re-surfacing dismissed slug: {slug} ({dismissed[slug]['traces_since']} traces since dismissal)")
        to_synthesize.append(("slug", slug, traces))

    # 2. Repeated pitfalls (min_freq+ occurrences)
    for key, traces in pitfall_freq.items():
        if len(traces) < min_freq:
            continue
        pitfall_slug = key.replace(" ", "-")[:30]
        if pitfall_slug in existing:
            continue
        to_synthesize.append(("pitfall", pitfall_slug, traces))

    # 3. Co-occurring slug pairs across min_freq+ traces
    pair_map = {}
    for slug, traces in slug_freq.items():
        trace_ids = {t["trace_id"] for t in traces}
        for other_slug, other_traces in slug_freq.items():
            if other_slug <= slug:
                continue
            overlap = trace_ids & {t["trace_id"] for t in other_traces}
            if len(overlap) >= min_freq:
                pair_key = f"{slug}+{other_slug}"
                if pair_key not in existing:
                    # Gather the overlapping traces
                    overlap_traces = [t for t in traces if t["trace_id"] in overlap]
                    pair_map[pair_key] = overlap_traces
    for pair_key, traces in pair_map.items():
        to_synthesize.append(("pair", pair_key, traces))

    if not to_synthesize:
        print("ctx-detect: no candidates found above threshold")
        return 0

    print(f"ctx-detect: {len(to_synthesize)} candidate(s) to synthesize")

    if dry_run:
        for kind, slug, traces in to_synthesize:
            trace_ids = [t["trace_id"] for t in traces]
            print(f"  [{kind}] {slug} — {len(traces)} traces: {', '.join(trace_ids[:5])}")
        return 0

    # Synthesize each candidate
    created = 0
    for kind, slug, traces in to_synthesize:
        print(f"\n  synthesizing [{kind}] {slug} ({len(traces)} traces)...")
        prompt = build_synthesis_prompt(slug, traces)

        try:
            raw = call(task="detect", prompt=prompt, system=DETECTION_SYSTEM_PROMPT,
                       max_tokens=1500)
            result = parse_json_response(raw)
        except (json.JSONDecodeError, ValueError):
            # Retry once
            try:
                raw = call(task="detect", prompt=prompt, system=DETECTION_SYSTEM_PROMPT,
                           max_tokens=1500)
                result = parse_json_response(raw)
            except (json.JSONDecodeError, ValueError) as e:
                print(f"  SKIP {slug} — JSON parse failed: {e}")
                continue
        except RuntimeError as e:
            print(f"  SKIP {slug} — model call failed: {e}")
            continue

        # Null escape hatch — model says no generalizable pattern
        if result.get("namespace") is None:
            print(f"  SKIP {slug} — model returned null (no generalizable pattern)")
            continue

        # Validate required keys
        required = ("namespace", "slug", "title", "body")
        if not all(k in result for k in required):
            print(f"  SKIP {slug} — missing keys in response")
            continue

        # Validate namespace
        valid_ns = {"skills/internal", "patterns", "debugging", "primitives"}
        if result["namespace"] not in valid_ns:
            print(f"  SKIP {slug} — invalid namespace: {result['namespace']}")
            continue

        write_candidate(conn, result, traces, len(traces))
        created += 1

    return created


def main():
    if not DB_PATH.exists():
        print("ctx-detect: index.db not found. Run ctx-ingest.py first.", file=sys.stderr)
        sys.exit(1)

    dry_run = "--dry-run" in sys.argv
    force   = "--force" in sys.argv

    conn = sqlite3.connect(DB_PATH)
    cfg  = _config()
    batch_size = cfg["detection"]["batch_size"]

    # Check trace counter gate (unless --force)
    if not force and not dry_run:
        row = conn.execute(
            "SELECT value FROM meta WHERE key='traces_since_detection'"
        ).fetchone()
        traces_since = int(row[0]) if row else 0

        # Count summarized traces as proxy if counter not set
        if traces_since == 0:
            total_summaries = conn.execute("SELECT COUNT(*) FROM summaries").fetchone()[0]
            row2 = conn.execute(
                "SELECT value FROM meta WHERE key='last_detection_run'"
            ).fetchone()
            if row2:
                traces_since = conn.execute(
                    "SELECT COUNT(*) FROM summaries WHERE summarized_at > ?",
                    (row2[0],)
                ).fetchone()[0]
            else:
                traces_since = total_summaries

        if traces_since < batch_size:
            print(f"ctx-detect: {traces_since}/{batch_size} traces since last detection. "
                  f"Use --force to run anyway.")
            conn.close()
            return

    print(f"ctx-detect: running detection...")
    created = run_detection(conn, dry_run=dry_run)

    if not dry_run and created >= 0:
        # Update meta
        now = datetime.now(timezone.utc).isoformat()
        conn.execute(
            "INSERT INTO meta (key, value) VALUES ('last_detection_run', ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (now,),
        )
        conn.execute(
            "INSERT INTO meta (key, value) VALUES ('traces_since_detection', '0') "
            "ON CONFLICT(key) DO UPDATE SET value='0'",
        )
        conn.commit()

    if created > 0:
        print(f"\nctx-detect: {created} candidate(s) created. Review with: ctx serve")
    conn.close()


if __name__ == "__main__":
    main()
