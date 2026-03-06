#!/usr/bin/env python3
# ~/.context/ctx-embed.py
# Embed knowledge across all namespaces.
# - Short items (patterns, debugging, skills, primitives) -> embeddings table
# - Long items (learnings, docs, plans, references) -> chunks table (split on ##)
# Uses all-MiniLM-L6-v2. Searches both tables at compile time.
#
# Usage:
#   python3 ctx-embed.py                    # embed all un-embedded summaries
#   python3 ctx-embed.py --all              # re-embed everything (force)
#   python3 ctx-embed.py <trace_id>         # embed a specific trace
#   python3 ctx-embed.py --dry-run          # show what would be embedded

import json, re, sqlite3, sys, struct
from datetime import datetime, timezone
from pathlib import Path

try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR = Path.home() / ".context"
DB_PATH     = CONTEXT_DIR / "index.db"
MODEL_NAME  = "all-MiniLM-L6-v2"
_model      = None


def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")


def get_model():
    global _model
    if _model is None:
        try:
            from sentence_transformers import SentenceTransformer
        except ImportError:
            print("ctx-embed: sentence-transformers not installed.", file=sys.stderr)
            print("  Install with: pip3 install sentence-transformers", file=sys.stderr)
            sys.exit(1)
        print(f"ctx-embed: loading {MODEL_NAME}...")
        _model = SentenceTransformer(MODEL_NAME)
    return _model


def embed_text(text: str) -> bytes:
    model = get_model()
    vector = model.encode(text, normalize_embeddings=True)
    return struct.pack(f"{len(vector)}f", *vector)


def estimate_tokens(text: str) -> int:
    """Rough token estimate: ~4 chars per token."""
    return len(text) // 4


def ensure_chunks_table(conn: sqlite3.Connection):
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


# ── Chunking ──────────────────────────────────────────────────────────────────

def chunk_document(content: str, max_tokens: int = 400, min_tokens: int = 50) -> list:
    """Split a document into chunks by ## headers. Returns list of (heading, text)."""
    # Split on ## headers
    parts = re.split(r'^(##\s+.+)$', content, flags=re.MULTILINE)

    chunks = []
    current_heading = "(preamble)"
    current_text = ""

    for part in parts:
        if re.match(r'^##\s+', part):
            # Save previous chunk if substantial
            if current_text.strip() and estimate_tokens(current_text) >= min_tokens:
                chunks.append((current_heading, current_text.strip()))
            current_heading = part.strip().lstrip("#").strip()
            current_text = ""
        else:
            current_text += part

    # Final chunk
    if current_text.strip() and estimate_tokens(current_text) >= min_tokens:
        chunks.append((current_heading, current_text.strip()))

    # If a chunk is too large, split on paragraphs
    result = []
    for heading, text in chunks:
        if estimate_tokens(text) <= max_tokens:
            result.append((heading, text))
        else:
            paragraphs = text.split("\n\n")
            current = ""
            for para in paragraphs:
                if estimate_tokens(current + para) > max_tokens and current:
                    result.append((heading, current.strip()))
                    current = para + "\n\n"
                else:
                    current += para + "\n\n"
            if current.strip() and estimate_tokens(current) >= min_tokens:
                result.append((heading, current.strip()))

    # If no chunks produced (doc too short), return as single chunk
    if not result and content.strip():
        result.append(("(full)", content.strip()))

    return result


def needs_chunking(namespace: str) -> bool:
    """Namespaces that use the chunks table."""
    return namespace in ("learnings", "docs", "plans", "references")


def get_namespace(path: str) -> str:
    """Derive namespace from a file path relative to CONTEXT_DIR."""
    if "learnings/" in path:
        return "learnings"
    if "docs/" in path:
        return "docs"
    if "plans/" in path:
        return "plans"
    if "references/" in path:
        return "references"
    if "patterns/" in path:
        return "patterns"
    if "debugging/" in path:
        return "debugging"
    if "skills/internal/" in path:
        return "skills/internal"
    if "skills/external/" in path:
        return "skills/external"
    if "primitives/" in path:
        return "primitives"
    return "other"


# ── Embed trace summaries (embeddings table) ─────────────────────────────────

def embed_trace(conn: sqlite3.Connection, trace_id: str, text: str) -> bool:
    try:
        blob = embed_text(text)
    except Exception as e:
        print(f"  SKIP {trace_id} — encoding failed: {e}", file=sys.stderr)
        return False
    conn.execute(
        "INSERT INTO embeddings (trace_id, vector) VALUES (?, ?) "
        "ON CONFLICT(trace_id) DO UPDATE SET vector=excluded.vector",
        (trace_id, blob),
    )
    conn.commit()
    return True


def get_unembedded_traces(conn: sqlite3.Connection, force_all: bool = False) -> list:
    if force_all:
        rows = conn.execute(
            "SELECT s.trace_id, s.embedding_text FROM summaries s "
            "WHERE s.embedding_text IS NOT NULL AND s.embedding_text != ''"
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT s.trace_id, s.embedding_text FROM summaries s "
            "LEFT JOIN embeddings e ON s.trace_id = e.trace_id "
            "WHERE e.trace_id IS NULL "
            "AND s.embedding_text IS NOT NULL AND s.embedding_text != ''"
        ).fetchall()
    return [{"trace_id": r[0], "embedding_text": r[1]} for r in rows]


# ── Embed namespace files (chunks or embeddings table) ───────────────────────

def embed_namespace_file(conn: sqlite3.Connection, filepath: Path, namespace: str,
                         source_id: str, max_tokens: int, min_tokens: int) -> int:
    """Embed a namespace file. Returns number of chunks/embeddings created."""
    content = filepath.read_text()
    rel_path = str(filepath.relative_to(CONTEXT_DIR))

    if needs_chunking(namespace) and estimate_tokens(content) > 300:
        # Chunk and embed into chunks table
        chunks = chunk_document(content, max_tokens, min_tokens)
        # Clear old chunks for this source
        conn.execute("DELETE FROM chunks WHERE source_id = ?", (source_id,))
        for i, (heading, text) in enumerate(chunks):
            chunk_id = f"{source_id}-chunk-{i}"
            try:
                blob = embed_text(text)
            except Exception as e:
                print(f"  SKIP chunk {chunk_id}: {e}", file=sys.stderr)
                continue
            conn.execute(
                "INSERT INTO chunks (id, source_id, source_path, namespace, chunk_index, heading, content, vector) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?) "
                "ON CONFLICT(id) DO UPDATE SET content=excluded.content, heading=excluded.heading, vector=excluded.vector",
                (chunk_id, source_id, rel_path, namespace, i, heading, text, blob),
            )
        conn.commit()
        return len(chunks)
    else:
        # Short doc — single embedding in embeddings table
        # Extract meaningful text (skip frontmatter)
        body = content
        if content.startswith("---"):
            parts = content.split("---", 2)
            body = parts[2].strip() if len(parts) >= 3 else content
        if not body.strip():
            return 0
        try:
            blob = embed_text(body[:1500])
        except Exception as e:
            print(f"  SKIP {source_id}: {e}", file=sys.stderr)
            return 0
        # Store in chunks table with chunk_index=0 for consistency
        conn.execute("DELETE FROM chunks WHERE source_id = ?", (source_id,))
        conn.execute(
            "INSERT INTO chunks (id, source_id, source_path, namespace, chunk_index, heading, content, vector) "
            "VALUES (?, ?, ?, ?, 0, '(full)', ?, ?) "
            "ON CONFLICT(id) DO UPDATE SET content=excluded.content, vector=excluded.vector",
            (f"{source_id}-chunk-0", source_id, rel_path, namespace, body[:1500], blob),
        )
        conn.commit()
        return 1


def scan_namespace_files() -> list:
    """Scan all namespace dirs for .md files to embed."""
    items = []
    scan_dirs = {
        "docs": "docs",
        "plans": "plans",
        "patterns": "patterns",
        "debugging": "debugging",
        "skills/internal": "skills/internal",
        "skills/external": "skills/external",
        "primitives": "primitives",
    }
    for subdir, namespace in scan_dirs.items():
        dir_path = CONTEXT_DIR / subdir
        if not dir_path.exists():
            continue
        for md in dir_path.rglob("*.md"):
            source_id = f"ns-{namespace.replace('/', '-')}-{md.stem}"
            items.append({
                "source_id": source_id,
                "path": md,
                "namespace": namespace,
            })
    return items


# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    if not DB_PATH.exists():
        print("ctx-embed: index.db not found. Run ctx-ingest.py first.", file=sys.stderr)
        sys.exit(1)

    conn = sqlite3.connect(DB_PATH)
    ensure_chunks_table(conn)

    cfg = _config()
    max_tokens = cfg.get("chunking", {}).get("max_chunk_tokens", 400)
    min_tokens = cfg.get("chunking", {}).get("min_chunk_tokens", 50)

    force_all = "--all" in sys.argv
    dry_run = "--dry-run" in sys.argv
    args = [a for a in sys.argv[1:] if not a.startswith("--")]

    # Specific trace
    if args:
        trace_id = args[0]
        row = conn.execute(
            "SELECT embedding_text FROM summaries WHERE trace_id = ?", (trace_id,)
        ).fetchone()
        if not row:
            print(f"ctx-embed: no summary found for {trace_id}", file=sys.stderr)
            sys.exit(1)
        ok = embed_trace(conn, trace_id, row[0])
        if ok:
            print(f"ctx-embed: {trace_id} embedded")
        conn.close()
        sys.exit(0 if ok else 1)

    # ── Phase 1: Trace summaries -> embeddings table ──
    trace_items = get_unembedded_traces(conn, force_all=force_all)

    # ── Phase 2: Namespace files -> chunks table ──
    ns_items = scan_namespace_files()
    if not force_all:
        # Only embed files not already chunked
        existing = {r[0] for r in conn.execute("SELECT DISTINCT source_id FROM chunks").fetchall()}
        ns_items = [it for it in ns_items if it["source_id"] not in existing]

    total = len(trace_items) + len(ns_items)
    if not total:
        print("ctx-embed: nothing to embed (use --all to re-embed)")
        conn.close()
        return

    if dry_run:
        print(f"ctx-embed: {len(trace_items)} trace(s) + {len(ns_items)} namespace file(s) to embed:")
        for it in trace_items:
            print(f"  [trace] {it['trace_id']}: {it['embedding_text'][:60]}...")
        for it in ns_items:
            print(f"  [{it['namespace']}] {it['path'].relative_to(CONTEXT_DIR)}")
        conn.close()
        return

    print(f"ctx-embed: {len(trace_items)} trace(s) + {len(ns_items)} namespace file(s)")

    ok_traces = 0
    for it in trace_items:
        if embed_trace(conn, it["trace_id"], it["embedding_text"]):
            ok_traces += 1
            print(f"  [trace] {it['trace_id']} done")

    ok_ns = 0
    chunk_total = 0
    for it in ns_items:
        n = embed_namespace_file(conn, it["path"], it["namespace"],
                                 it["source_id"], max_tokens, min_tokens)
        if n > 0:
            ok_ns += 1
            chunk_total += n
            print(f"  [{it['namespace']}] {it['path'].name} -> {n} chunk(s)")

    # ── Phase 3: Prune stale chunks for deleted files ──
    stale_sources = []
    all_chunks = conn.execute("SELECT DISTINCT source_id, source_path FROM chunks").fetchall()
    for source_id, source_path in all_chunks:
        if source_path and not (CONTEXT_DIR / source_path).exists():
            stale_sources.append((source_id, source_path))
    if stale_sources:
        for source_id, source_path in stale_sources:
            conn.execute("DELETE FROM chunks WHERE source_id = ?", (source_id,))
            print(f"  [pruned] {source_path} (file deleted)")
        conn.commit()

    now = datetime.now(timezone.utc).isoformat()
    conn.execute(
        "INSERT INTO meta (key, value) VALUES ('last_embed_ts', ?) "
        "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
        (now,),
    )
    conn.commit()

    print(f"ctx-embed: done — {ok_traces}/{len(trace_items)} traces, "
          f"{ok_ns}/{len(ns_items)} files ({chunk_total} chunks)"
          f"{f', pruned {len(stale_sources)}' if stale_sources else ''}")
    conn.close()


if __name__ == "__main__":
    main()
