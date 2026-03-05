#!/usr/bin/env python3
# ~/.context/ctx-ingest.py
# Parse raw JSON / Claude Code JSONL → learnings/*.md + SQLite traces table.
#
# Usage:
#   python3 ctx-ingest.py <conversation_id>          # ingest from raw/ (Cursor hook flow)
#   python3 ctx-ingest.py --claude-code <jsonl_path> # ingest a Claude Code JSONL session

import json, sqlite3, sys, hashlib, os
from datetime import datetime, timezone
from pathlib import Path

CONTEXT_DIR = Path.home() / ".context"
RAW_DIR     = CONTEXT_DIR / "raw"
LEARN_DIR   = CONTEXT_DIR / "learnings"
DB_PATH     = CONTEXT_DIR / "index.db"

# ── Schema ─────────────────────────────────────────────────────────────────────

SCHEMA = """
CREATE TABLE IF NOT EXISTS traces (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT,
    ts              TEXT,
    source          TEXT,
    status          TEXT,
    workspace       TEXT,
    project         TEXT,
    prompt          TEXT,
    files_modified  TEXT,
    loop_count      INTEGER,
    raw_path        TEXT
);

CREATE TABLE IF NOT EXISTS summaries (
    trace_id        TEXT PRIMARY KEY,
    problem         TEXT,
    pattern         TEXT,
    skills          TEXT,
    pitfalls        TEXT,
    embedding_text  TEXT,
    summarized_at   TEXT,
    FOREIGN KEY(trace_id) REFERENCES traces(id)
);

CREATE TABLE IF NOT EXISTS embeddings (
    trace_id        TEXT PRIMARY KEY,
    vector          BLOB,
    FOREIGN KEY(trace_id) REFERENCES traces(id)
);

CREATE TABLE IF NOT EXISTS candidates (
    id              TEXT PRIMARY KEY,
    type            TEXT,
    slug            TEXT,
    definition      TEXT,
    status          TEXT DEFAULT 'pending',
    frequency       INTEGER,
    source_traces   TEXT,
    detected_at     TEXT,
    resolved_at     TEXT
);

CREATE TABLE IF NOT EXISTS knowledge (
    id              TEXT PRIMARY KEY,
    namespace       TEXT,
    slug            TEXT,
    file_path       TEXT,
    summary         TEXT,
    tags            TEXT,
    source          TEXT,
    frequency       INTEGER DEFAULT 1,
    promoted_at     TEXT,
    vector          BLOB
);

CREATE TABLE IF NOT EXISTS meta (
    key             TEXT PRIMARY KEY,
    value           TEXT
);
"""


def init_db(conn: sqlite3.Connection):
    conn.executescript(SCHEMA)
    conn.commit()


# ── Cursor raw/ flow ───────────────────────────────────────────────────────────

def ingest_cursor(conversation_id: str):
    raw_files = sorted(RAW_DIR.glob(f"{conversation_id}_*.json"))
    if not raw_files:
        print(f"ctx-ingest: no raw files found for conversation_id={conversation_id}", file=sys.stderr)
        sys.exit(1)

    prompt        = ""
    files_edited  = []
    status        = "unknown"
    loop_count    = 0
    workspace     = ""
    ts            = datetime.now(timezone.utc).isoformat()

    for f in raw_files:
        try:
            data  = json.loads(f.read_text())
            event = data.get("hook_event_name", "")

            if event == "beforeSubmitPrompt":
                prompt    = data.get("prompt", "")
                workspace = (data.get("workspace_roots") or [""])[0]
                ts        = data.get("ts", ts)

            elif event == "afterFileEdit":
                fp = data.get("file_path", "")
                if fp and fp not in files_edited:
                    files_edited.append(fp)
                if not workspace:
                    workspace = (data.get("workspace_roots") or [""])[0]

            elif event == "stop":
                status     = data.get("status", "unknown")
                loop_count = data.get("loop_count", 0)
                if not workspace:
                    workspace = (data.get("workspace_roots") or [""])[0]

        except (json.JSONDecodeError, OSError) as e:
            print(f"ctx-ingest: error reading {f}: {e}", file=sys.stderr)

    project  = Path(workspace).name if workspace else ""
    short_id = conversation_id[:8]
    trace_id = f"trace-{short_id}"

    _write_learning_md(
        trace_id=trace_id,
        conversation_id=conversation_id,
        ts=ts,
        source="cursor",
        status=status,
        workspace=workspace,
        project=project,
        prompt=prompt,
        files_modified=files_edited,
        loop_count=loop_count,
        raw_path=str(RAW_DIR),
    )

    _upsert_trace(
        trace_id=trace_id,
        conversation_id=conversation_id,
        ts=ts,
        source="cursor",
        status=status,
        workspace=workspace,
        project=project,
        prompt=prompt,
        files_modified=files_edited,
        loop_count=loop_count,
        raw_path=str(RAW_DIR),
    )


# ── Claude Code JSONL flow ─────────────────────────────────────────────────────

def ingest_claude_code(jsonl_path: str):
    path = Path(jsonl_path)
    if not path.exists():
        print(f"ctx-ingest: file not found: {jsonl_path}", file=sys.stderr)
        sys.exit(1)

    lines = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                lines.append(json.loads(line))
            except json.JSONDecodeError:
                continue

    if not lines:
        print(f"ctx-ingest: empty or unparseable JSONL: {jsonl_path}", file=sys.stderr)
        return

    # Extract session metadata from first line
    first        = lines[0]
    session_id   = first.get("sessionId", path.stem)
    workspace    = first.get("cwd", "")
    project      = Path(workspace).name if workspace else ""

    # Reconstruct conversation
    user_messages      = []
    assistant_messages = []
    ts                 = first.get("timestamp", datetime.now(timezone.utc).isoformat())

    for entry in lines:
        etype = entry.get("type")
        if etype == "user":
            msg = entry.get("message", {})
            content = msg.get("content", "")
            if isinstance(content, list):
                for c in content:
                    if isinstance(c, dict) and c.get("type") == "text":
                        text = c.get("text", "").strip()
                        if text:
                            user_messages.append(text)
            elif isinstance(content, str) and content.strip():
                user_messages.append(content.strip())

        elif etype == "assistant":
            msg = entry.get("message", {})
            content = msg.get("content", [])
            if isinstance(content, list):
                for c in content:
                    if isinstance(c, dict) and c.get("type") == "text":
                        text = c.get("text", "").strip()
                        if text:
                            assistant_messages.append(text)

    # Derive stable ID from file path
    path_hash = hashlib.md5(str(path).encode()).hexdigest()[:8]
    trace_id  = f"trace-{path_hash}"
    short_id  = path_hash

    prompt = user_messages[0] if user_messages else ""

    # Derive date from first timestamp
    try:
        dt_str = ts[:10]  # YYYY-MM-DD
    except Exception:
        dt_str = datetime.now(timezone.utc).strftime("%Y-%m-%d")

    learn_name = f"claude-trace-{dt_str}-{short_id}.md"

    _write_learning_md(
        trace_id=trace_id,
        conversation_id=session_id,
        ts=ts,
        source="claude-code",
        status="completed",
        workspace=workspace,
        project=project,
        prompt=prompt,
        files_modified=[],
        loop_count=len(lines),
        raw_path=str(path),
        learn_name=learn_name,
        extra_content=_format_claude_code_body(user_messages, assistant_messages),
    )

    _upsert_trace(
        trace_id=trace_id,
        conversation_id=session_id,
        ts=ts,
        source="claude-code",
        status="completed",
        workspace=workspace,
        project=project,
        prompt=prompt,
        files_modified=[],
        loop_count=len(lines),
        raw_path=str(path),
    )


def _format_claude_code_body(user_msgs: list, assistant_msgs: list) -> str:
    parts = []
    if user_msgs:
        parts.append("## User Messages\n")
        for i, m in enumerate(user_msgs[:5], 1):  # cap at 5 for readability
            parts.append(f"**[{i}]** {m[:500]}\n")
    if assistant_msgs:
        parts.append("\n## Assistant Summary\n")
        # Include first assistant message as session opening context
        parts.append(assistant_msgs[0][:800] if assistant_msgs else "")
    return "\n".join(parts)


# ── Shared output ──────────────────────────────────────────────────────────────

def _write_learning_md(
    trace_id, conversation_id, ts, source, status,
    workspace, project, prompt, files_modified, loop_count, raw_path,
    learn_name=None, extra_content="",
):
    LEARN_DIR.mkdir(parents=True, exist_ok=True)

    if learn_name is None:
        try:
            dt_str = ts[:10]
        except Exception:
            dt_str = datetime.now(timezone.utc).strftime("%Y-%m-%d")
        short = conversation_id[:8] if conversation_id else trace_id[-8:]
        learn_name = f"cursor-trace-{dt_str}-{short}.md"

    out_path = LEARN_DIR / learn_name

    tags = [source + "-trace", status]
    frontmatter = f"""---
id: {trace_id}
conversation_id: {conversation_id}
ts: {ts}
source: {source}
status: {status}
workspace: {workspace}
project: {project}
tags: [{", ".join(tags)}]
---
"""

    body_lines = []
    if prompt:
        body_lines.append(f"## Prompt\n\n{prompt}\n")
    if files_modified:
        body_lines.append("## Files Modified\n")
        for fp in files_modified:
            body_lines.append(f"- {fp}")
        body_lines.append("")
    body_lines.append(f"## Session Info\n\nLoop count: {loop_count}  \nStatus: {status}")
    if extra_content:
        body_lines.append(f"\n{extra_content}")

    out_path.write_text(frontmatter + "\n" + "\n".join(body_lines) + "\n")
    print(f"ctx-ingest: wrote {out_path}")


def _upsert_trace(
    trace_id, conversation_id, ts, source, status,
    workspace, project, prompt, files_modified, loop_count, raw_path,
):
    conn = sqlite3.connect(DB_PATH)
    init_db(conn)
    conn.execute(
        """
        INSERT INTO traces (id, conversation_id, ts, source, status, workspace, project,
                            prompt, files_modified, loop_count, raw_path)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            ts=excluded.ts, status=excluded.status,
            prompt=excluded.prompt, files_modified=excluded.files_modified,
            loop_count=excluded.loop_count
        """,
        (
            trace_id, conversation_id, ts, source, status,
            workspace, project, prompt,
            json.dumps(files_modified), loop_count, raw_path,
        ),
    )
    conn.commit()
    conn.close()
    print(f"ctx-ingest: upserted trace {trace_id} → {DB_PATH}")


# ── Entry point ────────────────────────────────────────────────────────────────

def main():
    if len(sys.argv) < 2:
        print("Usage:")
        print("  ctx-ingest.py <conversation_id>           # Cursor raw/ flow")
        print("  ctx-ingest.py --claude-code <jsonl_path>  # Claude Code JSONL")
        sys.exit(1)

    if sys.argv[1] == "--claude-code":
        if len(sys.argv) < 3:
            print("ctx-ingest: --claude-code requires a path argument", file=sys.stderr)
            sys.exit(1)
        ingest_claude_code(sys.argv[2])
    else:
        ingest_cursor(sys.argv[1])


if __name__ == "__main__":
    main()
