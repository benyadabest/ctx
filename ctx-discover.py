#!/usr/bin/env python3
# ~/.context/ctx-discover.py
# Read-only scan of Cursor + Claude Code sessions. Reports counts, date ranges,
# workspace info, and estimated cost. Does NOT ingest anything.
#
# Usage:
#   python3 ctx-discover.py

import json, os, sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path.home() / ".context"))

try:
    import toml
except ImportError:
    import tomllib as toml

CONTEXT_DIR = Path.home() / ".context"
CURSOR_DIR  = Path.home() / ".cursor" / "projects"
CLAUDE_DIR  = Path.home() / ".claude" / "projects"


def _config():
    return toml.load(CONTEXT_DIR / "ctx.toml")


def _estimate_tokens(size_bytes: int) -> int:
    """Rough estimate: ~4 chars per token."""
    return size_bytes // 4


def _estimate_cost(total_tokens: int, model: str) -> str:
    """Estimate summarization cost. $0 for ollama models."""
    if model.startswith("ollama/"):
        return "$0.00 (local)"
    # Haiku: ~$0.25/M input + $1.25/M output. Summarize uses ~600 output tokens.
    # Rough estimate: input dominates at ~2000 tokens/trace avg
    if "haiku" in model:
        cost = (total_tokens / 1_000_000) * 0.25 + (total_tokens / 2000) * 600 * (1.25 / 1_000_000)
        return f"~${cost:.2f}"
    # Sonnet
    if "sonnet" in model:
        cost = (total_tokens / 1_000_000) * 3.0 + (total_tokens / 2000) * 600 * (15.0 / 1_000_000)
        return f"~${cost:.2f}"
    return "unknown"


def _project_from_workspace_hash(workspace_hash: str, sample_file: Path = None) -> str:
    """Extract a readable project name from a Cursor workspace dir name."""
    stripped = workspace_hash.replace("Users-benshvartsman-", "")
    noise = {"Library", "Application", "Support", "Cursor", "Workspaces", "workspace", "json"}
    parts = stripped.split("-")
    clean = [p for p in parts if p not in noise and not p.isdigit()]

    if clean and clean != ["Users", "benshvartsman"]:
        # Join meaningful path segments: "coding-chesscom-internship-coach-chatbot" → "coach-chatbot"
        return "-".join(clean[-2:]) if len(clean) > 1 else clean[-1]

    # Workspace hash is a temp workspace — peek inside transcript for working_directory
    if sample_file and sample_file.exists():
        try:
            text = sample_file.read_text(errors="replace")[:5000]
            import re
            match = re.search(r'working_directory:\s*(/[^\s\n]+)', text)
            if match:
                return Path(match.group(1)).name
            match = re.search(r'/Users/benshvartsman/coding/([^/"\s]+)', text)
            if match:
                return match.group(1)
        except OSError:
            pass

    return f"workspace-{workspace_hash[-8:]}"


def discover_cursor():
    if not CURSOR_DIR.exists():
        return [], {}

    files = []
    workspaces = {}

    for transcript_dir in sorted(CURSOR_DIR.glob("*/agent-transcripts")):
        workspace_hash = transcript_dir.parent.name
        txt_files = sorted(transcript_dir.glob("*.txt"))
        if not txt_files:
            continue

        # Derive project name from workspace dir name
        # e.g. "Users-benshvartsman-coding-chesscom-internship-coach-chatbot" → "coach-chatbot"
        # Cursor temp workspaces: "Users-benshvartsman-Library-Application-Support-..." → peek inside
        project = _project_from_workspace_hash(workspace_hash, txt_files[0])

        workspaces[workspace_hash] = {
            "project": project,
            "count": len(txt_files),
        }

        for f in txt_files:
            try:
                stat = f.stat()
                files.append({
                    "path": f,
                    "size": stat.st_size,
                    "mtime": datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc),
                    "workspace": workspace_hash,
                    "project": project,
                })
            except OSError:
                continue

    return files, workspaces


def discover_claude_code():
    if not CLAUDE_DIR.exists():
        return []

    files = []
    for jsonl in sorted(CLAUDE_DIR.glob("**/*.jsonl")):
        # Skip subagent files — they're part of a parent session
        if "/subagents/" in str(jsonl):
            continue
        try:
            stat = jsonl.stat()

            # Try to extract session metadata from first user message
            session_id = jsonl.stem
            workspace = ""
            project = ""
            ts = datetime.fromtimestamp(stat.st_mtime, tz=timezone.utc)

            with open(jsonl) as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        d = json.loads(line)
                        if d.get("type") == "user":
                            session_id = d.get("sessionId", session_id)
                            workspace = d.get("cwd", "")
                            project = Path(workspace).name if workspace else ""
                            t = d.get("timestamp")
                            if t:
                                ts = datetime.fromisoformat(t.replace("Z", "+00:00"))
                            break
                    except (json.JSONDecodeError, ValueError):
                        continue

            files.append({
                "path": jsonl,
                "size": stat.st_size,
                "mtime": ts,
                "session_id": session_id,
                "workspace": workspace,
                "project": project,
            })
        except OSError:
            continue

    return files


def main():
    cfg = _config()
    summarize_model = cfg["models"]["summarize"]

    # Cursor
    cursor_files, cursor_workspaces = discover_cursor()
    cursor_total_size = sum(f["size"] for f in cursor_files)
    cursor_tokens = _estimate_tokens(cursor_total_size)

    print("CURSOR TRANSCRIPTS")
    if cursor_files:
        dates = sorted(f["mtime"] for f in cursor_files)
        print(f"  Found: {len(cursor_files)} transcript files")
        print(f"  Across: {len(cursor_workspaces)} workspace(s)")
        for wh, info in cursor_workspaces.items():
            print(f"    {info['project']:<30} {info['count']} file(s)  [{wh[:40]}...]")
        print(f"  Date range: {dates[0].strftime('%Y-%m-%d')} → {dates[-1].strftime('%Y-%m-%d')}")
        print(f"  Total size: {cursor_total_size:,} bytes")
        print(f"  Estimated tokens: ~{cursor_tokens:,}")
    else:
        print("  None found")

    # Claude Code
    claude_files = discover_claude_code()
    claude_total_size = sum(f["size"] for f in claude_files)
    claude_tokens = _estimate_tokens(claude_total_size)

    print(f"\nCLAUDE CODE SESSIONS")
    if claude_files:
        dates = sorted(f["mtime"] for f in claude_files)
        projects = {}
        for f in claude_files:
            p = f["project"] or "(unknown)"
            projects[p] = projects.get(p, 0) + 1
        print(f"  Found: {len(claude_files)} session files (excluding subagents)")
        print(f"  Across: {len(projects)} project(s)")
        for p, count in sorted(projects.items(), key=lambda x: -x[1]):
            print(f"    {p:<30} {count} session(s)")
        print(f"  Date range: {dates[0].strftime('%Y-%m-%d')} → {dates[-1].strftime('%Y-%m-%d')}")
        print(f"  Total size: {claude_total_size:,} bytes")
        print(f"  Estimated tokens: ~{claude_tokens:,}")
    else:
        print("  None found")

    # Totals + cost
    total_tokens = cursor_tokens + claude_tokens
    total_files = len(cursor_files) + len(claude_files)
    cost = _estimate_cost(total_tokens, summarize_model)

    print(f"\nTOTAL")
    print(f"  Sessions: {total_files}")
    print(f"  Estimated tokens: ~{total_tokens:,}")
    print(f"  Summarization model: {summarize_model}")
    print(f"  Estimated summarization cost: {cost}")

    print(f"\nRECOMMENDATION")
    print(f"  Run: python3 ~/.context/ctx-backfill.py --dry-run   to preview what would be ingested")
    print(f"  Run: python3 ~/.context/ctx-backfill.py             to ingest all")
    print(f"  Run: python3 ~/.context/ctx-backfill.py --after 2025-01-01  to ingest selectively")


if __name__ == "__main__":
    main()
