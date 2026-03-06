# ctx build session

## Hard constraints
- All scripts go in ~/.context/
- No external dependencies beyond: watchdog, sentence-transformers, fastapi, uvicorn, toml
- model_router.py must be imported by every script that calls a model — no direct API calls elsewhere
- Every script must be independently runnable and testable
- SQLite schema changes must be additive only (no DROP TABLE)

## Model routing
Never hardcode a model name in any script.
Always import and use model_router.call(task, prompt, system).
Valid task values: "summarize", "detect", "compile", "synthesize".

## Namespace rules

### Directory structure
```
~/.context/
├── learnings/          AUTO. One .md per agent session. Never touch.
├── candidates/         AUTO. Pending approval. Never touch.
├── patterns/           Synthesized from docs only. Never from traces.
├── debugging/          Manual. Failure modes + fixes. Single .md files.
├── skills/
│   ├── internal/       Promoted from traces via detect.
│   │   └── {slug}/
│   │       ├── skill.md
│   │       └── references/   optional
│   └── external/       Synthesized from external sources or manual.
│       └── {slug}/
│           ├── skill.md
│           └── references/   optional
├── primitives/         Promoted. Atomic units. Single .md files.
├── docs/               Manual or pushed. Architecture notes, reference material.
│   └── {slug}.md or {slug}/doc.md + references/
└── plans/              Manual only. Flat dir, no subdirs.
    └── {slug}.md       Status is frontmatter field: active|completed|shelved
```

### Promotion rules (strictly enforced)
- traces      -> detect     -> skills/internal/ ONLY
- docs        -> synthesize -> patterns/ or skills/
- external    -> synthesize -> skills/external/
- patterns    promoted ONLY from docs via synthesize, NEVER from traces
- plans       manual only, system never writes here
- docs        manual or ctx push, system never auto-generates

## Phase gates
Stop and ask for confirmation after completing each phase.
Do not assume approval to continue.

## File output
Every script that produces output must print the output path on completion.
Every script must handle empty input or missing dependencies gracefully.

## Testing
After writing each script, output the exact terminal command to verify it works in isolation.
Do not move to the next script until the test command is confirmed.
