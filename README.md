# ctx

Personal knowledge infrastructure that captures AI dev sessions, indexes them, and compiles relevant context for future sessions.

## What it does

1. **Captures** Cursor and Claude Code sessions automatically via hooks and file watchers
2. **Summarizes** each session into portable patterns and domain-specific knowledge using LLM extraction
3. **Embeds** summaries and documents for semantic search
4. **Detects** recurring patterns across sessions and surfaces them as candidates for promotion
5. **Compiles** grounded context briefings — given a task description, returns an approach paragraph, relevant skills/learnings, and open questions drawn from your accumulated knowledge
6. **Synthesizes** new skills from external sources (URLs, docs) and existing knowledge

## Architecture

```
capture → ingest → summarize → embed → detect → compile
                                         ↓
                                    synthesize
```

All scripts are independent — no imports between them except `model_router.py`. SQLite is the only data store. Model routing is centralized through a single `call(task, prompt, system)` interface.

### Scripts

| Script | Purpose |
|---|---|
| `ctx` | CLI entry point |
| `model_router.py` | Routes LLM calls by task to configured model |
| `ctx-hook.py` | Cursor hook handler (stdin JSON → raw/) |
| `ctx-ingest.py` | Parse raw sessions → learnings/ + SQLite |
| `ctx-watcher.py` | Watches Claude Code sessions for changes |
| `ctx-summarize.py` | Two-target extraction: portable + domain |
| `ctx-embed.py` | Embedding + chunking (all-MiniLM-L6-v2) |
| `ctx-discover.py` | List available sessions without ingesting |
| `ctx-backfill.py` | Ingest historical sessions |
| `ctx-detect.py` | Pattern detection across traces |
| `ctx-compile.py` | Grounded briefing compiler |
| `ctx-synthesize.py` | Skill/pattern synthesis from sources |
| `ctx-serve.py` | Web UI (FastAPI, port 7337) |

### Namespaces

```
~/.context/
├── learnings/        # trace summaries (auto-generated)
├── candidates/       # detection output (pending approval)
├── patterns/         # promoted cross-project patterns
├── debugging/        # recurring debugging strategies
├── skills/internal/  # promoted from traces via detection
├── skills/external/  # synthesized from external sources
├── primitives/       # reusable code/config fragments
├── docs/             # reference documents
└── plans/            # implementation plans
```

## Setup

```bash
pip3 install toml watchdog sentence-transformers fastapi uvicorn
```

Create `~/.context/ctx.toml` with your config (see ctx.toml.example in the repo or run `ctx status` for defaults).

Set your API key:
```bash
export ANTHROPIC_API_KEY="sk-..."
```

## Usage

```bash
ctx status                              # pipeline health
ctx backfill --after 2026-01-01         # ingest historical sessions
ctx detect --force                      # run pattern detection
ctx compile "build X" --tag my-project  # compile context briefing
ctx push doc.md --namespace docs        # add document to knowledge base
ctx note "TIL: something useful"        # quick note
ctx serve                               # start web UI
```

## Dependencies

- Python 3.9+
- `toml` — config parsing
- `watchdog` — file system watching
- `sentence-transformers` — local embeddings
- `fastapi` + `uvicorn` — web UI
- Anthropic API key for LLM calls (or Ollama for offline mode)
