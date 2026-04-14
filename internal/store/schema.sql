-- Key-value entries (flat knowledge store)
CREATE TABLE IF NOT EXISTS entries (
    key         TEXT PRIMARY KEY NOT NULL,
    value       TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'text/plain',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
    key, value, content='entries', content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
    INSERT INTO entries_fts(rowid, key, value) VALUES (new.rowid, new.key, new.value);
END;

CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
    INSERT INTO entries_fts(entries_fts, rowid, key, value) VALUES ('delete', old.rowid, old.key, old.value);
END;

CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE ON entries BEGIN
    INSERT INTO entries_fts(entries_fts, rowid, key, value) VALUES ('delete', old.rowid, old.key, old.value);
    INSERT INTO entries_fts(rowid, key, value) VALUES (new.rowid, new.key, new.value);
END;

-- Traces: one row per agent session
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

-- Summaries: LLM-extracted knowledge per trace
CREATE TABLE IF NOT EXISTS summaries (
    trace_id        TEXT PRIMARY KEY REFERENCES traces(id),
    problem         TEXT,
    pattern         TEXT,
    skills          TEXT,
    pitfalls        TEXT,
    embedding_text  TEXT,
    project_tag     TEXT DEFAULT '',
    component       TEXT DEFAULT '',
    domain_knowledge TEXT DEFAULT '',
    summarized_at   TEXT
);

-- Embeddings: 384-dim float32 vectors for trace summaries
CREATE TABLE IF NOT EXISTS embeddings (
    trace_id        TEXT PRIMARY KEY REFERENCES traces(id),
    vector          BLOB
);

-- Candidates: detected patterns pending review
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

-- Knowledge: promoted items in namespace tree
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

-- Chunks: embedded document fragments
CREATE TABLE IF NOT EXISTS chunks (
    id              TEXT PRIMARY KEY,
    source_id       TEXT,
    source_path     TEXT,
    namespace       TEXT,
    chunk_index     INTEGER,
    heading         TEXT,
    content         TEXT,
    vector          BLOB
);

-- Synthesis history
CREATE TABLE IF NOT EXISTS synthesis_history (
    id              TEXT PRIMARY KEY,
    skill_path      TEXT,
    version         INTEGER,
    sources         TEXT,
    feedback        TEXT,
    synthesized_at  TEXT,
    model           TEXT
);

-- Usage: LLM API call tracking
CREATE TABLE IF NOT EXISTS usage (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              TEXT NOT NULL,
    task            TEXT NOT NULL,
    model           TEXT NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL NOT NULL DEFAULT 0.0,
    trace_id        TEXT
);

-- Meta: pipeline state
CREATE TABLE IF NOT EXISTS meta (
    key             TEXT PRIMARY KEY,
    value           TEXT
);
