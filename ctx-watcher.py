#!/usr/bin/env python3
# ~/.context/ctx-watcher.py
# Watches ~/.claude/projects/**/*.jsonl for new/changed Claude Code sessions.
# Uses watchdog if available, falls back to polling every 30s.
#
# Usage:
#   python3 ctx-watcher.py               # run as daemon
#   python3 ctx-watcher.py --list-sessions  # dry run, list found sessions

import sys, os, time, subprocess, hashlib
from pathlib import Path
from datetime import datetime, timezone

CONTEXT_DIR   = Path.home() / ".context"
CLAUDE_DIR    = Path.home() / ".claude" / "projects"
INGEST_SCRIPT = CONTEXT_DIR / "ctx-ingest.py"
POLL_INTERVAL = 30  # seconds

# Track already-processed files: path → mtime
_seen: dict = {}


def find_sessions() -> list[Path]:
    if not CLAUDE_DIR.exists():
        return []
    return sorted(CLAUDE_DIR.glob("**/*.jsonl"))


def list_sessions():
    sessions = find_sessions()
    print(f"Claude Code sessions found: {len(sessions)}")
    for s in sessions:
        size = s.stat().st_size
        mtime = datetime.fromtimestamp(s.stat().st_mtime, tz=timezone.utc).strftime("%Y-%m-%d %H:%M")
        print(f"  {mtime}  {size:>8} bytes  {s}")


def ingest_session(path: Path):
    if not INGEST_SCRIPT.exists():
        print(f"ctx-watcher: ctx-ingest.py not found at {INGEST_SCRIPT}", file=sys.stderr)
        return
    print(f"ctx-watcher: ingesting {path}")
    try:
        subprocess.run(
            [sys.executable, str(INGEST_SCRIPT), "--claude-code", str(path)],
            check=True,
            timeout=60,
        )
    except subprocess.CalledProcessError as e:
        print(f"ctx-watcher: ingest failed for {path}: {e}", file=sys.stderr)
    except subprocess.TimeoutExpired:
        print(f"ctx-watcher: ingest timed out for {path}", file=sys.stderr)


def check_sessions():
    """Check for new or modified sessions and ingest them."""
    for path in find_sessions():
        try:
            mtime = path.stat().st_mtime
        except OSError:
            continue
        prev_mtime = _seen.get(str(path))
        if prev_mtime is None or mtime > prev_mtime:
            _seen[str(path)] = mtime
            if prev_mtime is not None:  # only ingest on change, not initial scan
                ingest_session(path)
            # On first scan, just record without ingesting (avoid re-ingesting history on start)


def run_polling():
    print(f"ctx-watcher: polling {CLAUDE_DIR} every {POLL_INTERVAL}s (no watchdog)")
    # Initial scan — populate _seen without ingesting
    check_sessions()
    print(f"ctx-watcher: tracking {len(_seen)} existing session(s)")
    while True:
        time.sleep(POLL_INTERVAL)
        check_sessions()


def run_watchdog():
    from watchdog.observers import Observer
    from watchdog.events import FileSystemEventHandler, FileModifiedEvent, FileCreatedEvent

    class Handler(FileSystemEventHandler):
        def on_created(self, event):
            if not event.is_directory and event.src_path.endswith(".jsonl"):
                path = Path(event.src_path)
                _seen[str(path)] = path.stat().st_mtime
                # Don't ingest on creation — wait for a write/close (modified event)

        def on_modified(self, event):
            if not event.is_directory and event.src_path.endswith(".jsonl"):
                path = Path(event.src_path)
                try:
                    mtime = path.stat().st_mtime
                except OSError:
                    return
                prev = _seen.get(str(path))
                _seen[str(path)] = mtime
                if prev is not None:  # only ingest on subsequent modifications
                    ingest_session(path)

    if not CLAUDE_DIR.exists():
        print(f"ctx-watcher: {CLAUDE_DIR} does not exist, waiting...", file=sys.stderr)
        CLAUDE_DIR.mkdir(parents=True, exist_ok=True)

    # Populate _seen on startup without ingesting
    check_sessions()
    print(f"ctx-watcher: tracking {len(_seen)} existing session(s) via watchdog")

    observer = Observer()
    observer.schedule(Handler(), str(CLAUDE_DIR), recursive=True)
    observer.start()
    print(f"ctx-watcher: watching {CLAUDE_DIR}")
    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        observer.stop()
    observer.join()


def main():
    if "--list-sessions" in sys.argv:
        list_sessions()
        return

    try:
        import watchdog
        run_watchdog()
    except ImportError:
        print("ctx-watcher: watchdog not installed, using polling fallback")
        print("  Install with: pip3 install watchdog")
        run_polling()


if __name__ == "__main__":
    main()
