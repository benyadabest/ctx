#!/usr/bin/env python3
# ~/.context/ctx-summarize.py
# AI summarization of traces with two-target extraction:
#   PORTABLE: problem, pattern, skills[], pitfalls, embedding_text (no project nouns)
#   DOMAIN:   project_tag, component, knowledge (project-specific, for filtering)
#
# Uses model_router.call(task="summarize") with temperature=0.
# JSON parse includes a single retry on JSONDecodeError.
#
# Usage:
#   python3 ctx-summarize.py                    # summarize all unsummarized traces
#   python3 ctx-summarize.py --all              # reprocess ALL traces (wipe + redo)
#   python3 ctx-summarize.py <trace_id>         # summarize a specific trace
#   python3 ctx-summarize.py --dry-run          # show what would be summarized

import json, sqlite3, sys, re
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))
from model_router import call

CONTEXT_DIR = Path.home() / ".context"
LEARN_DIR   = CONTEXT_DIR / "learnings"
DB_PATH     = CONTEXT_DIR / "index.db"

SYSTEM_PROMPT = """You are a knowledge extraction system for a senior AI/ML engineer.
Extract two distinct types of knowledge from this agent session.
Respond ONLY with valid JSON. No markdown. No preamble."""

USER_TEMPLATE = """Extract from this agent session:

SESSION:
{session_block}

Return this exact JSON structure:
{{
  "portable": {{
    "problem": "One sentence. Abstract class of problem. Zero project-specific nouns.",
    "pattern": "1-2 sentences. Reusable approach. Forward-looking, not descriptive.",
    "skills": ["slug-1", "slug-2", "slug-3"],
    "pitfalls": "1-2 sentences. What to avoid regardless of project.",
    "embedding_text": "2-3 sentences. No project names, no product names, no repo names. Optimized for semantic search across future sessions."
  }},
  "domain": {{
    "project_tag": "slug of the project this belongs to, e.g. coach-chatbot",
    "component": "which subsystem or component, e.g. eval-pipeline",
    "knowledge": "What did this session reveal about HOW THIS SPECIFIC SYSTEM WORKS? Architecture decisions, implementation details, gotchas. Null if nothing project-specific was learned."
  }}
}}

RULES:
- portable.embedding_text must contain ZERO project-specific nouns
- skills must be specific: "judge-calibration" not "ai-improvement"
- If loop_count > 20, this was a deep session — reflect that depth
- pattern must be forward-looking guidance, not a description of what happened
- domain.knowledge is null if the session was generic/not project-specific"""


def ensure_domain_columns(conn: sqlite3.Connection):
    """Add domain columns to summaries table if they don't exist (additive schema)."""
    existing = {row[1] for row in conn.execute("PRAGMA table_info(summaries)").fetchall()}
    for col in ("project_tag", "component", "domain_knowledge"):
        if col not in existing:
            conn.execute(f"ALTER TABLE summaries ADD COLUMN {col} TEXT DEFAULT ''")
    conn.commit()


def build_extraction_prompt(trace: dict) -> str:
    parts = []
    parts.append(f"Prompt: {trace['prompt'][:2000]}" if trace['prompt'] else "Prompt: (none)")
    if trace['files_modified']:
        try:
            files = json.loads(trace['files_modified'])
            if files:
                parts.append(f"Files modified: {', '.join(files[:20])}")
        except (json.JSONDecodeError, TypeError):
            pass
    parts.append(f"Status: {trace['status']}")
    parts.append(f"Workspace: {trace['workspace']}")
    parts.append(f"Loop count: {trace['loop_count']}")
    session_block = "\n".join(parts)
    return USER_TEMPLATE.format(session_block=session_block)


def parse_json_response(raw: str) -> dict:
    """Parse JSON from model response. Tries direct parse, then strips markdown fences."""
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        pass
    stripped = re.sub(r'^```(?:json)?\s*', '', raw.strip())
    stripped = re.sub(r'\s*```$', '', stripped)
    return json.loads(stripped)


def summarize_trace(conn: sqlite3.Connection, trace: dict) -> bool:
    """Summarize a single trace. Returns True on success."""
    trace_id = trace['id']
    prompt = build_extraction_prompt(trace)

    # Attempt 1
    try:
        raw = call(task="summarize", prompt=prompt, system=SYSTEM_PROMPT,
                   max_tokens=800, temperature=0)
        result = parse_json_response(raw)
    except (json.JSONDecodeError, ValueError):
        # Retry once
        print(f"  retry {trace_id} (JSON parse failed)...", file=sys.stderr)
        try:
            raw = call(task="summarize", prompt=prompt, system=SYSTEM_PROMPT,
                       max_tokens=800, temperature=0)
            result = parse_json_response(raw)
        except (json.JSONDecodeError, ValueError) as e:
            print(f"  SKIP {trace_id} — JSON parse failed after retry: {e}", file=sys.stderr)
            print(f"  Raw: {raw[:200]}", file=sys.stderr)
            return False
    except RuntimeError as e:
        print(f"  SKIP {trace_id} — model call failed: {e}", file=sys.stderr)
        return False

    # Extract portable + domain blocks
    portable = result.get("portable", result)  # fallback: flat structure
    domain = result.get("domain", {})

    # Validate portable keys
    required = ("problem", "pattern", "skills", "pitfalls", "embedding_text")
    missing = [k for k in required if k not in portable]
    if missing:
        print(f"  SKIP {trace_id} — missing portable keys: {missing}", file=sys.stderr)
        return False

    # Normalize
    skills = portable["skills"]
    skills_json = json.dumps(skills if isinstance(skills, list) else [skills])
    project_tag = domain.get("project_tag", trace.get("project", "unknown") or "unknown")
    component = domain.get("component", "") or ""
    domain_knowledge = domain.get("knowledge") or ""  # null -> empty string

    now = datetime.now(timezone.utc).isoformat()

    # Upsert to summaries table
    conn.execute(
        """
        INSERT INTO summaries (trace_id, problem, pattern, skills, pitfalls, embedding_text,
                               project_tag, component, domain_knowledge, summarized_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(trace_id) DO UPDATE SET
            problem=excluded.problem, pattern=excluded.pattern,
            skills=excluded.skills, pitfalls=excluded.pitfalls,
            embedding_text=excluded.embedding_text,
            project_tag=excluded.project_tag, component=excluded.component,
            domain_knowledge=excluded.domain_knowledge,
            summarized_at=excluded.summarized_at
        """,
        (trace_id, portable["problem"], portable["pattern"], skills_json,
         portable["pitfalls"], portable["embedding_text"],
         project_tag, component, domain_knowledge, now),
    )
    conn.commit()

    # Update the learning .md file
    _update_learning_md(trace_id, portable, domain, project_tag)

    # Update meta
    conn.execute(
        "INSERT INTO meta (key, value) VALUES ('last_summarize_ts', ?) "
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        (now,),
    )
    conn.commit()

    print(f"  {trace_id} → {portable['skills']}  [{project_tag}]")
    return True


def _update_learning_md(trace_id: str, portable: dict, domain: dict, project_tag: str):
    """Update the learning .md: add project_tag to frontmatter, write summary block."""
    md_files = list(LEARN_DIR.glob("*.md"))
    for md_path in md_files:
        content = md_path.read_text()
        if f"id: {trace_id}" not in content:
            continue

        # Add project_tag to frontmatter if not present
        if "project_tag:" not in content:
            content = content.replace(
                f"id: {trace_id}",
                f"id: {trace_id}\nproject_tag: {project_tag}",
            )

        # Remove old AI Summary block if present (for --all reprocessing)
        content = re.sub(r'\n## AI Summary\n.*', '', content, flags=re.DOTALL)

        skills_str = ', '.join(portable['skills']) if isinstance(portable['skills'], list) else portable['skills']
        component = domain.get("component", "")
        knowledge = domain.get("knowledge", "")

        summary_block = f"""
## AI Summary

**Problem**: {portable['problem']}
**Pattern**: {portable['pattern']}
**Skills**: {skills_str}
**Pitfalls**: {portable['pitfalls']}

> {portable['embedding_text']}
"""
        if component or knowledge:
            summary_block += f"""
## Domain

**Project**: {project_tag}
**Component**: {component}
**Knowledge**: {knowledge}
"""

        md_path.write_text(content.rstrip() + "\n" + summary_block)
        return


def get_traces(conn: sqlite3.Connection, all_traces: bool = False) -> list:
    """Return traces to summarize. If all_traces, return everything."""
    if all_traces:
        query = """
            SELECT id, conversation_id, ts, source, status,
                   workspace, project, prompt, files_modified, loop_count
            FROM traces ORDER BY ts
        """
    else:
        query = """
            SELECT t.id, t.conversation_id, t.ts, t.source, t.status,
                   t.workspace, t.project, t.prompt, t.files_modified, t.loop_count
            FROM traces t
            LEFT JOIN summaries s ON t.id = s.trace_id
            WHERE s.trace_id IS NULL
            ORDER BY t.ts
        """
    rows = conn.execute(query).fetchall()
    cols = ["id", "conversation_id", "ts", "source", "status",
            "workspace", "project", "prompt", "files_modified", "loop_count"]
    return [dict(zip(cols, row)) for row in rows]


def main():
    if not DB_PATH.exists():
        print("ctx-summarize: index.db not found. Run ctx-ingest.py first.", file=sys.stderr)
        sys.exit(1)

    conn = sqlite3.connect(DB_PATH)
    ensure_domain_columns(conn)

    all_mode = "--all" in sys.argv
    dry_run = "--dry-run" in sys.argv
    args = [a for a in sys.argv[1:] if not a.startswith("--")]

    # Specific trace
    if args:
        trace_id = args[0]
        row = conn.execute(
            "SELECT id, conversation_id, ts, source, status, workspace, project, "
            "prompt, files_modified, loop_count FROM traces WHERE id = ?",
            (trace_id,),
        ).fetchone()
        if not row:
            print(f"ctx-summarize: trace {trace_id} not found", file=sys.stderr)
            sys.exit(1)
        cols = ["id", "conversation_id", "ts", "source", "status",
                "workspace", "project", "prompt", "files_modified", "loop_count"]
        trace = dict(zip(cols, row))
        ok = summarize_trace(conn, trace)
        conn.close()
        sys.exit(0 if ok else 1)

    # Batch mode
    traces = get_traces(conn, all_traces=all_mode)

    if not traces:
        print("ctx-summarize: all traces already summarized (use --all to reprocess)")
        conn.close()
        return

    if dry_run:
        print(f"ctx-summarize: {len(traces)} trace(s) to process:")
        for t in traces:
            p = t['prompt'][:60] if t['prompt'] else '(no prompt)'
            print(f"  {t['id']}  {t['source']}  {t['project']}  {p}")
        conn.close()
        return

    label = "reprocessing" if all_mode else "processing"
    print(f"ctx-summarize: {label} {len(traces)} trace(s)")
    ok_count = 0
    for i, t in enumerate(traces, 1):
        print(f"[{i}/{len(traces)}]", end="")
        if summarize_trace(conn, t):
            ok_count += 1

    print(f"\nctx-summarize: done — {ok_count}/{len(traces)} succeeded")
    conn.close()


if __name__ == "__main__":
    main()
