#!/usr/bin/env python3
# ~/.context/ctx-backfill.py
# Backfill historical Cursor transcripts + Claude Code sessions through the
# ingest → summarize → embed pipeline. Safe to re-run (deduplicates by trace ID).
#
# Usage:
#   python3 ctx-backfill.py                         # ingest all
#   python3 ctx-backfill.py --dry-run               # preview only
#   python3 ctx-backfill.py --limit 5               # first 5 only
#   python3 ctx-backfill.py --after 2025-06-01      # date filter
#   python3 ctx-backfill.py --source cursor          # cursor only
#   python3 ctx-backfill.py --source claude-code     # claude-code only

import json, sqlite3, sys, hashlib, subprocess, os
from datetime import datetime, timezone
from pathlib import Path

CONTEXT_DIR = Path.home() / ".context"
CURSOR_DIR  = Path.home() / ".cursor" / "projects"
CLAUDE_DIR  = Path.home() / ".claude" / "projects"
DB_PATH     = CONTEXT_DIR / "index.db"
SRC_DIR     = CONTEXT_DIR / "src"
INGEST      = SRC_DIR / "ctx-ingest.py"
SUMMARIZE   = SRC_DIR / "ctx-summarize.py"
EMBED       = SRC_DIR / "ctx-embed.py"
PYTHON      = sys.executable


def existing_trace_ids() -> set:
    """Return set of trace IDs already in the DB."""
    if not DB_PATH.exists():
        return set()
    conn = sqlite3.connect(DB_PATH)
    try:
        rows = conn.execute("SELECT id FROM traces").fetchall()
        return {r[0] for r in rows}
    except sqlite3.OperationalError:
        return set()
    finally:
        conn.close()


def trace_id_for_path(path: Path) -> str:
    """Derive the same stable trace ID that ctx-ingest.py would produce."""
    path_hash = hashlib.md5(str(path).encode()).hexdigest()[:8]
    return f"trace-{path_hash}"


def discover_cursor_files(after_date: str = None) -> list:
    """Find all Cursor agent-transcript .txt files."""
    if not CURSOR_DIR.exists():
        return []

    files = []
    for txt in sorted(CURSOR_DIR.glob("*/agent-transcripts/*.txt")):
        try:
            stat = txt.stat()
            mtime = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)

            if after_date:
                if mtime.strftime("%Y-%m-%d") < after_date:
                    continue

            # Derive workspace/project from dir name
            parent = txt.parent.parent.name
            parts = parent.replace("Users-benshvartsman-", "").split("-")
            noise = {"Library", "Application", "Support", "Cursor", "Workspaces", "workspace", "json"}
            clean = [p for p in parts if p not in noise and not p.isdigit()]
            project = clean[-1] if clean else parent[:20]

            files.append({
                "path": txt,
                "source": "cursor",
                "mtime": mtime,
                "size": stat.st_size,
                "project": project,
                "trace_id": trace_id_for_path(txt),
            })
        except OSError:
            continue

    return files


def discover_claude_files(after_date: str = None) -> list:
    """Find all Claude Code .jsonl session files (excluding subagents)."""
    if not CLAUDE_DIR.exists():
        return []

    files = []
    for jsonl in sorted(CLAUDE_DIR.glob("**/*.jsonl")):
        if "/subagents/" in str(jsonl):
            continue
        try:
            stat = jsonl.stat()
            mtime = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)

            if after_date:
                if mtime.strftime("%Y-%m-%d") < after_date:
                    continue

            # Try to get project from first user message
            project = ""
            with open(jsonl) as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        d = json.loads(line)
                        if d.get("type") == "user":
                            cwd = d.get("cwd", "")
                            project = Path(cwd).name if cwd else ""
                            break
                    except (json.JSONDecodeError, ValueError):
                        continue

            files.append({
                "path": jsonl,
                "source": "claude-code",
                "mtime": mtime,
                "size": stat.st_size,
                "project": project or jsonl.stem[:12],
                "trace_id": trace_id_for_path(jsonl),
            })
        except OSError:
            continue

    return files


def run_pipeline(file_info: dict, index: int, total: int) -> bool:
    """Run ingest → summarize → embed for a single file. Returns True on success."""
    path = file_info["path"]
    source = file_info["source"]
    trace_id = file_info["trace_id"]
    project = file_info["project"]

    prefix = f"[{index}/{total}] {trace_id} ({source}, {project})"

    # Step 1: Ingest
    if source == "cursor":
        cmd = [PYTHON, str(INGEST), "--cursor-transcript", str(path)]
    else:
        cmd = [PYTHON, str(INGEST), "--claude-code", str(path)]

    result = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    if result.returncode != 0:
        print(f"  {prefix} INGEST FAILED: {result.stderr.strip()}")
        return False
    print(f"  {prefix} ingested")

    # Step 2: Summarize
    result = subprocess.run(
        [PYTHON, str(SUMMARIZE), trace_id],
        capture_output=True, text=True, timeout=180,
    )
    if result.returncode != 0:
        stderr = result.stderr.strip()
        # Not a fatal error — trace is still ingested
        print(f"  {prefix} summarize skipped: {stderr[:100]}")
    else:
        # Print the skills extracted
        for line in result.stdout.strip().split("\n"):
            if "→" in line:
                print(f"  {prefix} {line.strip()}")

    # Step 3: Embed (only if summarized)
    result = subprocess.run(
        [PYTHON, str(EMBED), trace_id],
        capture_output=True, text=True, timeout=120,
    )
    if result.returncode != 0:
        stderr = result.stderr.strip()
        if "sentence-transformers" in stderr:
            pass  # Expected if not installed
        elif "no summary" in stderr:
            pass  # Expected if summarize failed
        else:
            print(f"  {prefix} embed skipped: {stderr[:80]}")
    else:
        print(f"  {prefix} embedded")

    return True


def main():
    args = sys.argv[1:]
    dry_run = "--dry-run" in args
    limit = None
    after_date = None
    source_filter = None

    i = 0
    while i < len(args):
        if args[i] == "--limit" and i + 1 < len(args):
            limit = int(args[i + 1])
            i += 2
        elif args[i] == "--after" and i + 1 < len(args):
            after_date = args[i + 1]
            i += 2
        elif args[i] == "--source" and i + 1 < len(args):
            source_filter = args[i + 1]
            i += 2
        else:
            i += 1

    # Discover files
    all_files = []
    if source_filter != "claude-code":
        all_files.extend(discover_cursor_files(after_date))
    if source_filter != "cursor":
        all_files.extend(discover_claude_files(after_date))

    # Sort by mtime
    all_files.sort(key=lambda f: f["mtime"])

    # Deduplicate: skip files whose trace_id is already in DB
    existing = existing_trace_ids()
    new_files = [f for f in all_files if f["trace_id"] not in existing]
    skipped = len(all_files) - len(new_files)

    if limit:
        new_files = new_files[:limit]

    print(f"ctx-backfill: {len(all_files)} total sessions found, "
          f"{skipped} already ingested, {len(new_files)} to process")

    if after_date:
        print(f"  filter: after {after_date}")
    if source_filter:
        print(f"  filter: source={source_filter}")
    if limit:
        print(f"  filter: limit={limit}")

    if not new_files:
        print("ctx-backfill: nothing to do")
        return

    if dry_run:
        print(f"\nDry run — would process {len(new_files)} session(s):")
        for f in new_files:
            date = f["mtime"].strftime("%Y-%m-%d")
            print(f"  {f['trace_id']}  {f['source']:<12} {date}  {f['project']:<20} {f['size']:>8} bytes")
        return

    print(f"\nProcessing {len(new_files)} session(s)...\n")
    ok_count = 0
    for i, f in enumerate(new_files, 1):
        try:
            if run_pipeline(f, i, len(new_files)):
                ok_count += 1
        except subprocess.TimeoutExpired:
            print(f"  [{i}/{len(new_files)}] {f['trace_id']} TIMEOUT")
        except Exception as e:
            print(f"  [{i}/{len(new_files)}] {f['trace_id']} ERROR: {e}")

    print(f"\nctx-backfill: done — {ok_count}/{len(new_files)} succeeded")

    # Update meta
    if ok_count > 0 and DB_PATH.exists():
        conn = sqlite3.connect(DB_PATH)
        now = datetime.now(timezone.utc).isoformat()
        conn.execute(
            "INSERT INTO meta (key, value) VALUES ('last_backfill_ts', ?) "
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (now,),
        )
        conn.commit()
        conn.close()


if __name__ == "__main__":
    main()
