#!/usr/bin/env python3
# ~/.context/ctx-compile.py
# Compile a grounded briefing from all knowledge for a given task.
# Searches both embeddings table (traces) and chunks table (namespace files).
# Generates unified approach paragraph + open questions via model_router.
# Output: ~/.context/compiled/{slug}.md + structured JSON for UI.
#
# Usage:
#   python3 ctx-compile.py "design a decomposable judge metric"
#   python3 ctx-compile.py --tag coach-chatbot "extend evals"
#   python3 ctx-compile.py --tags
#   python3 ctx-compile.py --watch          # watch plans/ for auto-compile

import json, math, re, sqlite3, struct, sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))

try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR  = Path.home() / ".context"
DB_PATH      = CONTEXT_DIR / "index.db"
COMPILED_DIR = CONTEXT_DIR / "compiled"

MODEL_NAME = "all-MiniLM-L6-v2"
_embed_model = None


def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")


def _get_embed_model():
    global _embed_model
    if _embed_model is None:
        from sentence_transformers import SentenceTransformer
        _embed_model = SentenceTransformer(MODEL_NAME)
    return _embed_model


def embed_query(text: str) -> list:
    return _get_embed_model().encode(text, normalize_embeddings=True).tolist()


def unpack_vector(blob: bytes) -> list:
    n = len(blob) // 4
    return list(struct.unpack(f"{n}f", blob))


def cosine_sim(a: list, b: list) -> float:
    return sum(x * y for x, y in zip(a, b))


def slugify(text: str) -> str:
    s = re.sub(r'[^a-z0-9\s-]', '', text.lower().strip())
    s = re.sub(r'[\s]+', '-', s)
    return re.sub(r'-+', '-', s)[:60].rstrip('-')


def recency_score(ts_str: str, halflife_days: float) -> float:
    if not ts_str:
        return 0.0
    try:
        ts = datetime.fromisoformat(ts_str.replace("Z", "+00:00"))
    except (ValueError, TypeError):
        return 0.0
    days_ago = max(0, (datetime.now(timezone.utc) - ts).total_seconds() / 86400)
    return math.exp(-0.693 * days_ago / halflife_days)


# ── Search both tables ────────────────────────────────────────────────────────

def search_traces(conn, query_vec, cfg, tag=None):
    """Search embeddings table (trace summaries)."""
    query = """
        SELECT t.id, t.ts, t.source, t.project, t.prompt,
               s.problem, s.pattern, s.skills, s.embedding_text,
               e.vector, s.project_tag, s.component, s.domain_knowledge
        FROM traces t
        JOIN summaries s ON t.id = s.trace_id
        JOIN embeddings e ON t.id = e.trace_id
    """
    params = []
    if tag:
        query += " WHERE s.project_tag = ?"
        params.append(tag)

    rows = conn.execute(query, params).fetchall()
    halflife = cfg["ranking"]["recency_halflife_days"]
    max_freq = 1

    items = []
    for r in rows:
        vec = unpack_vector(r[9]) if r[9] else None
        sim = cosine_sim(query_vec, vec) if vec else 0.0
        rec = recency_score(r[1], halflife)
        score = 0.60 * sim + 0.25 * (1.0 / max_freq) + 0.15 * rec
        items.append({
            "type": "learning", "id": r[0], "ts": r[1], "project": r[3],
            "problem": r[5], "pattern": r[6], "skills": r[7],
            "embedding_text": r[8], "project_tag": r[10] or "",
            "component": r[11] or "", "domain_knowledge": r[12] or "",
            "file_path": _find_learning_file(r[0]),
            "score": score, "sim": sim, "heading": None,
            "description": r[8] or r[5] or "",
        })
    items.sort(key=lambda x: -x["score"])
    return items


def search_chunks(conn, query_vec, cfg):
    """Search chunks table (namespace files)."""
    rows = conn.execute(
        "SELECT id, source_id, source_path, namespace, chunk_index, heading, content, vector "
        "FROM chunks WHERE vector IS NOT NULL"
    ).fetchall()

    halflife = cfg["ranking"]["recency_halflife_days"]
    # Group by source to get best chunk per source
    by_source = {}
    for r in rows:
        vec = unpack_vector(r[7])
        sim = cosine_sim(query_vec, vec)
        source_id = r[1]
        if source_id not in by_source or sim > by_source[source_id]["sim"]:
            ns = r[3]
            item_type = {
                "patterns": "pattern", "debugging": "debugging",
                "skills/internal": "skill", "skills/external": "skill_external",
                "skill_internal": "skill", "skill_external": "skill_external",
                "primitives": "primitive", "docs": "doc",
                "plans": "plan", "learnings": "learning_chunk",
            }.get(ns, "doc")
            by_source[source_id] = {
                "type": item_type, "id": source_id,
                "source_path": r[2], "file_path": r[2],
                "namespace": ns, "heading": r[5],
                "content": r[6][:300], "sim": sim,
                "description": r[6][:150],
                "score": 0.60 * sim + 0.25 * 0.5 + 0.15 * 0.5,
                "ts": None, "frequency": 1,
            }

    items = list(by_source.values())
    items.sort(key=lambda x: -x["score"])
    return items


def _find_learning_file(trace_id: str) -> str:
    for md in (CONTEXT_DIR / "learnings").glob("*.md"):
        if f"id: {trace_id}" in md.read_text()[:500]:
            return str(md.relative_to(CONTEXT_DIR))
    return f"learnings/{trace_id}.md"


# ── Briefing generation ───────────────────────────────────────────────────────

BRIEFING_SYSTEM = """You are a context synthesis engine for a senior AI/ML engineer.
Given a task and relevant knowledge pieces, produce a unified briefing.
Be specific, actionable, and grounded in the provided sources.
Do not hallucinate. Only use what is in the provided sources."""


def generate_briefing(task, ranked_items):
    from model_router import call

    knowledge_text = []
    for i, it in enumerate(ranked_items[:12], 1):
        desc = it.get("description", it.get("embedding_text", ""))[:200]
        heading = f" (section: {it['heading']})" if it.get("heading") and it["heading"] != "(full)" else ""
        knowledge_text.append(f"{i}. [{it['type']}] {it.get('file_path', it['id'])}{heading}\n   {desc}")

    prompt = f"""Task: {task}

Relevant knowledge:
{chr(10).join(knowledge_text)}

Produce:
1. A unified approach paragraph (3-5 sentences) that synthesizes the most relevant knowledge into concrete guidance for this task. This is not a summary of the sources — it is actionable guidance grounded in them.

2. Open questions: what is still unresolved or unknown that matters for this task? Be specific. Max 4 questions.

Respond with JSON:
{{
  "approach": "3-5 sentence paragraph",
  "open_questions": ["question 1", "question 2"]
}}"""

    try:
        raw = call(task="compile", prompt=prompt, system=BRIEFING_SYSTEM,
                   max_tokens=800, temperature=0)
        # Parse JSON
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            stripped = re.sub(r'^```(?:json)?\s*', '', raw.strip())
            stripped = re.sub(r'\s*```$', '', stripped)
            return json.loads(stripped)
    except Exception as e:
        return {
            "approach": f"(Could not generate briefing: {e})",
            "open_questions": [],
        }


# ── Output ────────────────────────────────────────────────────────────────────

def build_output(task, tag, ranked, briefing, cfg):
    """Build both markdown and structured JSON."""
    now = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M")
    top_k = cfg["compile"]

    # Group by type
    patterns   = [it for it in ranked if it["type"] == "pattern"][:top_k.get("top_k_patterns", 2)]
    skills     = [it for it in ranked if it["type"] in ("skill", "skill_external")][:top_k.get("top_k_skills", 3)]
    docs       = [it for it in ranked if it["type"] == "doc"][:top_k.get("top_k_docs", 2)]
    learnings  = [it for it in ranked if it["type"] == "learning"][:top_k.get("top_k_learnings", 3)]
    primitives = [it for it in ranked if it["type"] == "primitive"][:top_k.get("top_k_primitives", 2)]

    all_sources = []
    source_num = [0]
    def add_source(it):
        source_num[0] += 1
        all_sources.append({
            "path": it.get("file_path", it.get("source_path", it["id"])),
            "score": round(it.get("sim", 0), 2),
            "description": it.get("description", "")[:150],
        })
        return source_num[0]

    # Build markdown
    lines = [
        "# Context Bundle",
        f"**Task**: {task}",
    ]
    if tag:
        lines.append(f"**Filter**: project_tag={tag}")
    lines.extend([
        f"**Compiled**: {now}",
        f"**Sources**: {len(patterns) + len(skills) + len(docs) + len(learnings) + len(primitives)} knowledge items",
        "", "---", "",
        "## Approach", "",
        briefing.get("approach", ""),
    ])

    def render_section(title, items, prefix=""):
        if not items:
            return
        lines.extend(["", f"## {title}"])
        for it in items:
            add_source(it)
            fp = it.get("file_path", it.get("source_path", it["id"]))
            score = f"[{it.get('sim', 0):.2f}]"
            heading = f"  ## {it['heading']}" if it.get("heading") and it["heading"] not in ("(full)", "(preamble)") else ""
            extra = ""
            if it["type"] == "skill" and it.get("frequency", 1) > 1:
                extra = f"  [{it['frequency']} sessions]"
            if it["type"] == "learning" and it.get("project_tag"):
                extra = f"  [{it['project_tag']}]"
            lines.append(f"-> @{fp}  {score}{extra}{heading}")
            desc = it.get("description", it.get("embedding_text", ""))[:200]
            lines.append(f'  "{desc}"')
            lines.append("")

    render_section("Relevant Patterns", patterns)
    render_section("Relevant Skills", skills)
    render_section("Relevant Docs", docs)
    render_section("Relevant Learnings", learnings)
    render_section("Primitives", primitives)

    # Open questions
    lines.extend(["---", "", "## Open Questions"])
    for q in briefing.get("open_questions", []):
        lines.append(f"- {q}")

    # Sources
    lines.extend(["", "---", "", "## Sources"])
    for i, s in enumerate(all_sources, 1):
        lines.append(f"{i}. {s['path']} [{s['score']}]")
    lines.append("")

    md = "\n".join(lines)

    # Build JSON
    structured = {
        "query": task,
        "tag": tag,
        "approach": briefing.get("approach", ""),
        "sections": {
            "patterns": [_item_json(it) for it in patterns],
            "skills": [_item_json(it) for it in skills],
            "docs": [_item_json(it) for it in docs],
            "learnings": [_item_json(it) for it in learnings],
            "primitives": [_item_json(it) for it in primitives],
        },
        "open_questions": briefing.get("open_questions", []),
        "sources": all_sources,
    }

    return md, structured


def _item_json(it):
    return {
        "path": it.get("file_path", it.get("source_path", it["id"])),
        "score": round(it.get("sim", 0), 2),
        "heading": it.get("heading"),
        "description": it.get("description", "")[:200],
        "type": it["type"],
        "project_tag": it.get("project_tag", ""),
    }


# ── Main compile ─────────────────────────────────────────────────────────────

def compile_task(task: str, tag: str = None) -> tuple:
    """Returns (md_path, structured_json)."""
    cfg = _config()
    conn = sqlite3.connect(DB_PATH)

    effective_task = task if task else f"all knowledge for {tag}"

    print("ctx-compile: embedding query...")
    query_vec = embed_query(effective_task)

    print("ctx-compile: searching knowledge...")
    trace_items = search_traces(conn, query_vec, cfg, tag=tag)
    chunk_items = search_chunks(conn, query_vec, cfg)

    # Merge and deduplicate
    all_items = trace_items + chunk_items
    # Skip learning_chunk type if we already have the trace version
    trace_ids = {it["id"] for it in trace_items}
    all_items = [it for it in all_items if not (it["type"] == "learning_chunk" and
                 any(it.get("source_path", "").replace("learnings/", "trace-") in tid for tid in trace_ids))]

    all_items.sort(key=lambda x: -x["score"])
    print(f"ctx-compile: {len(trace_items)} traces + {len(chunk_items)} chunks = {len(all_items)} items")

    print("ctx-compile: generating briefing...")
    briefing = generate_briefing(effective_task, all_items)

    md, structured = build_output(effective_task, tag, all_items, briefing, cfg)

    COMPILED_DIR.mkdir(parents=True, exist_ok=True)
    slug = slugify(effective_task)
    out_path = COMPILED_DIR / f"{slug}.md"
    out_path.write_text(md)

    # Also write JSON for UI
    json_path = COMPILED_DIR / f"{slug}.json"
    json_path.write_text(json.dumps(structured, indent=2))

    print(f"ctx-compile: wrote {out_path}")
    conn.close()
    return str(out_path), structured


def get_all_tags(conn):
    rows = conn.execute(
        "SELECT DISTINCT project_tag FROM summaries WHERE project_tag != '' ORDER BY project_tag"
    ).fetchall()
    return [r[0] for r in rows]


# ── Watch mode ────────────────────────────────────────────────────────────────

def watch_plans():
    plans_dir = CONTEXT_DIR / "plans"
    plans_dir.mkdir(parents=True, exist_ok=True)

    try:
        from watchdog.observers import Observer
        from watchdog.events import FileSystemEventHandler
    except ImportError:
        print("ctx-compile: watchdog required for --watch.", file=sys.stderr)
        sys.exit(1)

    import time

    class PlanHandler(FileSystemEventHandler):
        def on_created(self, event):
            if not event.is_directory and event.src_path.endswith(".md"):
                path = Path(event.src_path)
                print(f"\nctx-compile: new plan detected: {path.name}")
                content = path.read_text()
                # Extract project from frontmatter
                tag = None
                m = re.search(r'^project:\s*(.+)', content, re.MULTILINE)
                if m:
                    tag = m.group(1).strip()
                # Use plan body as task
                parts = content.split("---")
                body = parts[-1].strip() if len(parts) >= 3 else content[:500]
                task = ""
                for line in body.split("\n"):
                    line = line.strip()
                    if line and not line.startswith("#"):
                        task = line[:300]
                        break
                if task:
                    compile_task(task, tag)

    observer = Observer()
    observer.schedule(PlanHandler(), str(plans_dir), recursive=False)
    observer.start()
    print(f"ctx-compile: watching {plans_dir} for new plans...")
    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        observer.stop()
    observer.join()


# ── CLI ───────────────────────────────────────────────────────────────────────

def main():
    if len(sys.argv) < 2:
        print("Usage:")
        print('  ctx-compile.py "task description"                compile a briefing')
        print('  ctx-compile.py --tag coach-chatbot "task"        compile filtered by project')
        print('  ctx-compile.py --tags                            list all project tags')
        print('  ctx-compile.py --watch                           watch plans/ for auto-compile')
        sys.exit(1)

    if not DB_PATH.exists():
        print("ctx-compile: index.db not found.", file=sys.stderr)
        sys.exit(1)

    if sys.argv[1] == "--tags":
        conn = sqlite3.connect(DB_PATH)
        for t in get_all_tags(conn):
            print(f"  {t}")
        conn.close()
        return

    if sys.argv[1] == "--watch":
        watch_plans()
        return

    # Parse --tag
    tag = None
    args = sys.argv[1:]
    if "--tag" in args:
        idx = args.index("--tag")
        if idx + 1 < len(args):
            tag = args[idx + 1]
            args = args[:idx] + args[idx+2:]

    task = " ".join(args) if args else ""
    if not task and not tag:
        print("ctx-compile: provide a task or --tag", file=sys.stderr)
        sys.exit(1)

    compile_task(task, tag)


if __name__ == "__main__":
    main()
