#!/usr/bin/env python3
# ~/.context/ctx-synthesize.py
# Synthesize skills or patterns from source material into the knowledge base.
# Checks for similar existing knowledge. Supports refinement of existing skills.
# Chunks + embeds the result. Logs to synthesis_history table.
#
# Usage:
#   python3 ctx-synthesize.py --source <url|filepath> --output <ns/slug> [--type skill|pattern]
#   python3 ctx-synthesize.py --source <url> --source <file> --output skills/external/eval-design
#   python3 ctx-synthesize.py --refine skills/internal/eval-design --source <url> --feedback "add calibration"
#   python3 ctx-synthesize.py --context "I build LLM coaching systems" --source <url> --output skills/external/coaching

import json, hashlib, re, sqlite3, struct, sys, urllib.request, urllib.error
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))

try:
    import toml
except ImportError:
    import tomllib as toml

from model_router import call

CONTEXT_DIR = Path.home() / ".context"
DB_PATH     = CONTEXT_DIR / "index.db"
MODEL_NAME  = "all-MiniLM-L6-v2"
_embed_model = None


def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")


def _get_embed_model():
    global _embed_model
    if _embed_model is None:
        from sentence_transformers import SentenceTransformer
        _embed_model = SentenceTransformer(MODEL_NAME)
    return _embed_model


def embed_text(text: str) -> bytes:
    model = _get_embed_model()
    vector = model.encode(text, normalize_embeddings=True)
    return struct.pack(f"{len(vector)}f", *vector)


def unpack_vector(blob: bytes) -> list:
    n = len(blob) // 4
    return list(struct.unpack(f"{n}f", blob))


def cosine_sim(a, b):
    return sum(x * y for x, y in zip(a, b))


def ensure_tables(conn: sqlite3.Connection):
    conn.execute("""
        CREATE TABLE IF NOT EXISTS synthesis_history (
            id              TEXT PRIMARY KEY,
            skill_path      TEXT,
            version         INTEGER,
            sources         TEXT,
            feedback        TEXT,
            synthesized_at  TEXT,
            model           TEXT
        )
    """)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS chunks (
            id           TEXT PRIMARY KEY,
            source_id    TEXT,
            source_path  TEXT,
            namespace    TEXT,
            chunk_index  INTEGER,
            heading      TEXT,
            content      TEXT,
            vector       BLOB
        )
    """)
    conn.commit()


# ── Source fetching ───────────────────────────────────────────────────────────

def fetch_source(source: str) -> tuple:
    """Fetch source content. Returns (label, content)."""
    if source.startswith("http://") or source.startswith("https://"):
        return _fetch_url(source)
    # Try as path relative to CONTEXT_DIR first, then absolute
    p = CONTEXT_DIR / source
    if not p.exists():
        p = Path(source).expanduser().resolve()
    if not p.exists():
        print(f"  source not found: {source}", file=sys.stderr)
        return (source, "")
    return (source, p.read_text())


def _fetch_url(url: str) -> tuple:
    """Fetch URL content, strip HTML if needed."""
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "ctx/1.0"})
        with urllib.request.urlopen(req, timeout=20) as r:
            ct = r.headers.get("Content-Type", "")
            body = r.read().decode("utf-8", errors="replace")
        if "html" in ct:
            body = re.sub(r'<script[^>]*>.*?</script>', '', body, flags=re.DOTALL)
            body = re.sub(r'<style[^>]*>.*?</style>', '', body, flags=re.DOTALL)
            body = re.sub(r'<[^>]+>', '', body)
            body = re.sub(r'\n{3,}', '\n\n', body)
        return (url, body[:8000])
    except Exception as e:
        print(f"  failed to fetch {url}: {e}", file=sys.stderr)
        return (url, "")


# ── Similarity check ─────────────────────────────────────────────────────────

def check_existing_similarity(conn: sqlite3.Connection, text: str, threshold: float) -> list:
    """Check if similar knowledge already exists. Returns list of (path, score)."""
    query_blob = embed_text(text)
    query_vec = unpack_vector(query_blob)

    matches = []

    # Check chunks table
    rows = conn.execute("SELECT source_id, source_path, vector FROM chunks WHERE vector IS NOT NULL").fetchall()
    seen_sources = set()
    for source_id, source_path, blob in rows:
        if source_id in seen_sources:
            continue
        seen_sources.add(source_id)
        vec = unpack_vector(blob)
        sim = cosine_sim(query_vec, vec)
        if sim >= threshold:
            matches.append((source_path or source_id, sim))

    # Check embeddings table (trace summaries)
    rows = conn.execute(
        "SELECT s.trace_id, s.embedding_text, e.vector FROM summaries s "
        "JOIN embeddings e ON s.trace_id = e.trace_id WHERE e.vector IS NOT NULL"
    ).fetchall()
    for trace_id, emb_text, blob in rows:
        vec = unpack_vector(blob)
        sim = cosine_sim(query_vec, vec)
        if sim >= threshold:
            matches.append((f"learnings/{trace_id}", sim))

    matches.sort(key=lambda x: -x[1])
    return matches[:5]


# ── Synthesis ─────────────────────────────────────────────────────────────────

SYNTH_SYSTEM = """You are a skill extraction system for a senior AI/ML engineer
building LLM systems, agent infrastructure, and evaluation pipelines.
Extract reusable, actionable knowledge from the provided source material.
Respond ONLY with valid JSON."""

def build_synth_prompt(sources_combined: str, context: str, synth_type: str,
                       existing_content: str = None, feedback: str = None) -> str:
    parts = [f"Source material:\n{sources_combined}"]

    if context:
        parts.append(f"\nEngineer context:\n{context}")
    if existing_content:
        parts.append(f"\nExisting skill to refine:\n{existing_content}")
    if feedback:
        parts.append(f"\nWhat to improve:\n{feedback}")

    type_label = "skill" if synth_type == "skill" else "pattern"
    parts.append(f"""
Produce a {type_label} definition:
{{
  "name": "hyphenated-slug",
  "description": "one sentence",
  "when_to_use": "what situations call for this",
  "process": ["step 1", "step 2", "step 3"],
  "key_principles": ["principle 1", "principle 2", "principle 3"],
  "pitfalls": ["pitfall 1", "pitfall 2"],
  "tags": ["tag1", "tag2"],
  "sources_used": ["url or filename for each source"]
}}""")
    return "\n".join(parts)


def write_skill_md(output_dir: Path, result: dict, synth_type: str,
                   version: int, source_labels: list) -> Path:
    """Write skill.md or pattern.md to output dir."""
    output_dir.mkdir(parents=True, exist_ok=True)

    name = result.get("name", output_dir.name)
    now = datetime.now(timezone.utc).isoformat()[:10]
    tags_str = "[" + ", ".join(result.get("tags", [])) + "]"
    sources_str = "[" + ", ".join(source_labels) + "]"

    frontmatter = f"""---
id: {synth_type}-{name}
type: {synth_type}
version: {version}
synthesized_at: {now}
sources: {sources_str}
tags: {tags_str}
---"""

    process = "\n".join(f"{i+1}. {s}" for i, s in enumerate(result.get("process", [])))
    principles = "\n".join(f"- {p}" for p in result.get("key_principles", []))
    pitfalls = "\n".join(f"- {p}" for p in result.get("pitfalls", []))
    sources_section = "\n".join(f"- {s}" for s in result.get("sources_used", source_labels))

    filename = "skill.md" if synth_type == "skill" else "pattern.md"
    filepath = output_dir / filename

    content = f"""{frontmatter}

# {name.replace('-', ' ').title()}

{result.get('description', '')}

## When to Use

{result.get('when_to_use', '')}

## Process

{process}

## Key Principles

{principles}

## Pitfalls

{pitfalls}

## Sources

{sources_section}
"""
    filepath.write_text(content)
    return filepath


def chunk_and_embed(conn: sqlite3.Connection, filepath: Path, source_id: str, namespace: str):
    """Chunk a file and embed into chunks table."""
    # Import chunking from ctx-embed
    content = filepath.read_text()
    rel_path = str(filepath.relative_to(CONTEXT_DIR))

    # Simple chunking: split on ## headers
    parts = re.split(r'^(##\s+.+)$', content, flags=re.MULTILINE)
    chunks = []
    heading = "(preamble)"
    text = ""
    for part in parts:
        if re.match(r'^##\s+', part):
            if text.strip() and len(text) > 50:
                chunks.append((heading, text.strip()))
            heading = part.strip().lstrip("#").strip()
            text = ""
        else:
            text += part
    if text.strip() and len(text) > 50:
        chunks.append((heading, text.strip()))
    if not chunks and content.strip():
        chunks = [("(full)", content.strip())]

    conn.execute("DELETE FROM chunks WHERE source_id = ?", (source_id,))
    for i, (h, t) in enumerate(chunks):
        chunk_id = f"{source_id}-chunk-{i}"
        try:
            blob = embed_text(t)
        except Exception:
            continue
        conn.execute(
            "INSERT OR REPLACE INTO chunks (id, source_id, source_path, namespace, chunk_index, heading, content, vector) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (chunk_id, source_id, rel_path, namespace, i, h, t, blob),
        )
    conn.commit()
    return len(chunks)


def add_to_knowledge(conn: sqlite3.Connection, source_id: str, namespace: str,
                     slug: str, filepath: Path, description: str, tags: list):
    """Add or update the knowledge table."""
    now = datetime.now(timezone.utc).isoformat()
    conn.execute(
        "INSERT INTO knowledge (id, namespace, slug, file_path, summary, tags, source, frequency, promoted_at) "
        "VALUES (?, ?, ?, ?, ?, ?, 'synthesized', 1, ?) "
        "ON CONFLICT(id) DO UPDATE SET summary=excluded.summary, tags=excluded.tags, promoted_at=excluded.promoted_at",
        (source_id, namespace, slug, str(filepath), description, json.dumps(tags), now),
    )
    conn.commit()


def store_references(output_dir: Path, sources: list, fetched: list):
    """Store external URL sources as reference files."""
    refs_dir = output_dir / "references"
    any_stored = False
    for label, content in fetched:
        if label.startswith("http") and content:
            refs_dir.mkdir(parents=True, exist_ok=True)
            slug = re.sub(r'https?://', '', label)
            slug = re.sub(r'[^a-z0-9]+', '-', slug.lower()).strip('-')[:40]
            ref_path = refs_dir / f"{slug}.md"
            ref_path.write_text(f"# Source: {label}\n\n{content[:5000]}\n")
            any_stored = True
    if any_stored:
        print(f"  references stored in {refs_dir.relative_to(CONTEXT_DIR)}/")


# ── CLI parsing ───────────────────────────────────────────────────────────────

def parse_args():
    args = sys.argv[1:]
    sources = []
    output = None
    synth_type = "skill"
    context = ""
    refine = None
    feedback = None

    i = 0
    while i < len(args):
        if args[i] in ("--source", "-s"):
            sources.append(args[i+1]); i += 2
        elif args[i] == "--output":
            output = args[i+1]; i += 2
        elif args[i] == "--type":
            synth_type = args[i+1]; i += 2
        elif args[i] == "--context":
            context = args[i+1]; i += 2
        elif args[i] == "--refine":
            refine = args[i+1]; i += 2
        elif args[i] == "--feedback":
            feedback = args[i+1]; i += 2
        else:
            i += 1

    return sources, output, synth_type, context, refine, feedback


USAGE = """ctx-synthesize: synthesize skills/patterns from source material

Usage:
  ctx-synthesize --source <url|file> --output <namespace/slug> [--type skill|pattern]
  ctx-synthesize --source <url> --source <file> --output skills/external/eval-design
  ctx-synthesize --refine skills/internal/eval-design --source <url> --feedback "improve X"

Options:
  --source <url|filepath>   source material (repeatable)
  --output <ns/slug>        output path, e.g. skills/external/eval-design
  --type skill|pattern      default: skill
  --context "string"        calibration context about your work
  --refine <skill-path>     refine existing skill instead of creating new
  --feedback "string"       what to improve (used with --refine)
"""


def main():
    sources, output, synth_type, context, refine, feedback = parse_args()

    if not sources and not refine:
        print(USAGE)
        sys.exit(1)

    conn = sqlite3.connect(DB_PATH)
    ensure_tables(conn)

    cfg = _config()
    sim_threshold = cfg.get("synthesize", {}).get("similarity_threshold", 0.85)

    # Handle refine mode
    existing_content = None
    version = 1
    if refine:
        refine_path = CONTEXT_DIR / refine
        if not refine_path.exists():
            # Try adding skill.md
            refine_path = CONTEXT_DIR / refine / "skill.md"
        if not refine_path.exists():
            print(f"ctx-synthesize: cannot find {refine}", file=sys.stderr)
            sys.exit(1)
        existing_content = refine_path.read_text()
        # Extract version from frontmatter
        m = re.search(r'^version:\s*(\d+)', existing_content, re.MULTILINE)
        version = int(m.group(1)) + 1 if m else 2
        if not output:
            # Derive output from refine path
            output = str(refine_path.parent.relative_to(CONTEXT_DIR))
        print(f"ctx-synthesize: refining {refine} -> v{version}")

    if not output and not refine:
        print("ctx-synthesize: --output required (or use --refine)", file=sys.stderr)
        sys.exit(1)

    # Fetch all sources
    print(f"ctx-synthesize: fetching {len(sources)} source(s)...")
    fetched = []
    source_labels = []
    for s in sources:
        label, content = fetch_source(s)
        if content:
            fetched.append((label, content))
            source_labels.append(label)
            print(f"  {label[:60]}... ({len(content)} chars)")
        else:
            print(f"  SKIP {label} (empty)")

    if not fetched and not existing_content:
        print("ctx-synthesize: no source content to synthesize", file=sys.stderr)
        sys.exit(1)

    # Combine sources
    sources_combined = ""
    for label, content in fetched:
        sources_combined += f"\n--- Source: {label} ---\n{content[:4000]}\n"

    # Check for similar existing knowledge
    check_text = sources_combined[:2000]
    similar = check_existing_similarity(conn, check_text, sim_threshold)
    if similar and not refine:
        print(f"\n  Similar knowledge exists:")
        for path, score in similar[:3]:
            print(f"    {path} ({score:.2f})")
        print(f"  Proceeding with synthesis (use --refine to update existing)")
        print()

    # Synthesize
    print(f"ctx-synthesize: calling model (task=synthesize)...")
    prompt = build_synth_prompt(sources_combined, context, synth_type,
                                existing_content, feedback)
    try:
        raw = call(task="synthesize", prompt=prompt, system=SYNTH_SYSTEM,
                   max_tokens=1500, temperature=0)
    except RuntimeError as e:
        print(f"ctx-synthesize: model call failed: {e}", file=sys.stderr)
        sys.exit(1)

    # Parse JSON
    try:
        result = json.loads(raw)
    except json.JSONDecodeError:
        stripped = re.sub(r'^```(?:json)?\s*', '', raw.strip())
        stripped = re.sub(r'\s*```$', '', stripped)
        try:
            result = json.loads(stripped)
        except json.JSONDecodeError as e:
            print(f"ctx-synthesize: JSON parse failed: {e}", file=sys.stderr)
            print(f"  Raw: {raw[:300]}", file=sys.stderr)
            sys.exit(1)

    # Write output
    output_dir = CONTEXT_DIR / output
    filepath = write_skill_md(output_dir, result, synth_type, version, source_labels)
    print(f"ctx-synthesize: wrote {filepath.relative_to(CONTEXT_DIR)}")

    # Store references for URLs
    store_references(output_dir, sources, fetched)

    # Determine namespace for knowledge table
    if "skills/internal" in output:
        ns = "skill_internal"
    elif "skills/external" in output:
        ns = "skill_external"
    elif "patterns" in output:
        ns = "pattern"
    else:
        ns = "skill_external"

    slug = output_dir.name
    source_id = f"synth-{ns}-{slug}"

    # Chunk + embed
    print(f"ctx-synthesize: chunking + embedding...")
    n_chunks = chunk_and_embed(conn, filepath, source_id, ns)
    print(f"  {n_chunks} chunk(s)")

    # Add to knowledge table
    description = result.get("description", "")
    tags = result.get("tags", [])
    add_to_knowledge(conn, source_id, ns, slug, filepath, description, tags)

    # Log to synthesis_history
    now = datetime.now(timezone.utc).isoformat()
    model = cfg["models"].get("synthesize", "unknown")
    hist_id = f"synth-{hashlib.md5(now.encode()).hexdigest()[:8]}"
    conn.execute(
        "INSERT INTO synthesis_history (id, skill_path, version, sources, feedback, synthesized_at, model) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        (hist_id, str(filepath.relative_to(CONTEXT_DIR)), version,
         json.dumps(source_labels), feedback or "", now, model),
    )
    conn.commit()
    conn.close()

    print(f"ctx-synthesize: done")


if __name__ == "__main__":
    main()
