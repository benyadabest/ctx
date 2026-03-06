# ctx — AI Agent Skill

How to use the ctx personal knowledge infrastructure from within an AI agent session.

## When to Use

Use ctx when you want to:
- Capture a learning, pattern, or insight from the current session
- Look up relevant context before starting a task
- Add external documentation to the knowledge base
- Tag or organize existing knowledge

## Commands

### Capture knowledge during a session

```bash
# Quick note — captures a learning and auto-embeds it for future retrieval
ctx note "Pattern: always validate schema at system boundaries, not in business logic"

# Tag an existing file with additional metadata
ctx tag learnings/cursor-trace-2026-02-18-abc123.md eval-design coaching
```

### Add external knowledge

```bash
# Push a local file into a namespace
ctx push path/to/doc.md --namespace docs

# Push a URL (auto-fetches and stores)
ctx push https://example.com/article --namespace docs

# Synthesize a skill from sources
ctx synthesize --source https://example.com/guide \
               --source ./local-reference.md \
               --output skills/external/new-skill \
               --type skill
```

### Retrieve context before a task

```bash
# Compile a grounded briefing for a task
ctx compile "design a rate limiter" --tag my-project

# Output: ~/.context/compiled/design-a-rate-limiter.md
# Contains: approach paragraph, relevant skills, learnings, open questions
```

### Pipeline operations

```bash
ctx status          # check pipeline health, counts, processes
ctx detect --force  # run pattern detection across all traces
ctx embed --all     # re-embed all knowledge
ctx serve           # start web UI at http://localhost:7337
```

## Valid Namespaces for Push

| Namespace | What goes here |
|---|---|
| `patterns` | Cross-project recurring patterns |
| `debugging` | Debugging strategies and solutions |
| `skills/internal` | Skills promoted from trace detection |
| `skills/external` | Skills synthesized from external sources |
| `primitives` | Reusable code/config fragments |
| `docs` | Reference documents |
| `plans` | Implementation plans |

## Key Behaviors

- `ctx note` creates a learning in `learnings/`, inserts a trace, auto-summarizes, and embeds — the note becomes searchable immediately
- `ctx push` copies the file, chunks+embeds it, and adds it to the knowledge table
- `ctx compile` searches both embeddings and chunks tables, ranks by 60% similarity + 25% frequency + 15% recency, and generates an approach paragraph grounded in your actual knowledge
- `ctx forget <trace-id>` fully removes a trace from all tables (traces, summaries, embeddings, chunks, knowledge) and deletes the learning file
- `ctx tag` updates both the file's frontmatter and the knowledge table

## Integration Pattern

At the start of a coding session, compile context for the task:
```bash
ctx compile "what I'm about to build" --tag project-name
cat ~/.context/compiled/what-im-about-to-build.md
```

During a session, capture insights:
```bash
ctx note "Discovered that X library requires Y configuration for Z use case"
```

After finding useful reference material:
```bash
ctx push https://useful-docs.com/guide --namespace docs
```
