#!/usr/bin/env python3
# ~/.context/ctx-hook.py
# Cursor hook handler. Reads JSON from stdin, writes to raw/, triggers ingest on stop.
#
# Test:
#   echo '{"conversation_id":"test-abc","hook_event_name":"stop","status":"completed","loop_count":1,"workspace_roots":["/tmp"]}' \
#     | python3 ~/.context/ctx-hook.py

import json, sys, os, subprocess
from datetime import datetime, timezone
from pathlib import Path

CONTEXT_DIR = Path.home() / ".context"
RAW_DIR     = CONTEXT_DIR / "raw"

def main():
    RAW_DIR.mkdir(parents=True, exist_ok=True)

    try:
        data = json.loads(sys.stdin.read())
    except json.JSONDecodeError as e:
        print(f"ctx-hook: invalid JSON from stdin: {e}", file=sys.stderr)
        sys.exit(1)

    conversation_id = data.get("conversation_id", "unknown")
    event           = data.get("hook_event_name", "unknown")
    ts              = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")

    out_path = RAW_DIR / f"{conversation_id}_{event}_{ts}.json"
    out_path.write_text(json.dumps(data, indent=2))
    print(f"ctx-hook: wrote {out_path}", file=sys.stderr)

    if event == "stop":
        ingest_script = CONTEXT_DIR / "src" / "ctx-ingest.py"
        if ingest_script.exists():
            subprocess.Popen(
                [sys.executable, str(ingest_script), conversation_id],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
            print(f"ctx-hook: spawned ctx-ingest.py {conversation_id}", file=sys.stderr)
        else:
            print(f"ctx-hook: ctx-ingest.py not found, skipping ingest", file=sys.stderr)


if __name__ == "__main__":
    main()
