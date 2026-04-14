package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$`)

type Entry struct {
	Key         string
	Value       string
	ContentType string
	CreatedAt   string
	UpdatedAt   string
}

type Store struct {
	db *sql.DB
}

func New(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %s: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	if len(key) > 255 {
		return fmt.Errorf("key too long (max 255 characters)")
	}
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("invalid key %q: must be dot-separated lowercase alphanumeric segments (e.g. react.hooks.patterns)", key)
	}
	return nil
}

func (s *Store) Get(key string) (*Entry, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}

	row := s.db.QueryRow(
		"SELECT key, value, content_type, created_at, updated_at FROM entries WHERE key = ?",
		key,
	)

	var e Entry
	if err := row.Scan(&e.Key, &e.Value, &e.ContentType, &e.CreatedAt, &e.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	return &e, nil
}

func (s *Store) Set(key, value, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	_, err := s.db.Exec(`
		INSERT INTO entries (key, value, content_type)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			content_type = excluded.content_type,
			updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
	`, key, value, contentType)
	if err != nil {
		return fmt.Errorf("set %q: %w", key, err)
	}
	return nil
}

func (s *Store) Delete(key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	_, err := s.db.Exec("DELETE FROM entries WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

func (s *Store) List(prefix string) ([]Entry, error) {
	var rows *sql.Rows
	var err error

	if prefix == "" {
		rows, err = s.db.Query("SELECT key, value, content_type, created_at, updated_at FROM entries ORDER BY key")
	} else {
		rows, err = s.db.Query(
			"SELECT key, value, content_type, created_at, updated_at FROM entries WHERE key GLOB ? OR key = ? ORDER BY key",
			prefix+".*", prefix,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Key, &e.Value, &e.ContentType, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *Store) Search(query string) ([]Entry, error) {
	rows, err := s.db.Query(`
		SELECT e.key, e.value, e.content_type, e.created_at, e.updated_at
		FROM entries_fts f
		JOIN entries e ON f.rowid = e.rowid
		WHERE entries_fts MATCH ?
		ORDER BY rank
	`, query)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Key, &e.Value, &e.ContentType, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("search scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ── Trace types and methods ──────────────────────────────────────────────────

type Trace struct {
	ID             string
	ConversationID string
	TS             string
	Source         string
	Status         string
	Workspace      string
	Project        string
	Prompt         string
	FilesModified  string // JSON array
	LoopCount      int
	RawPath        string
}

func (s *Store) UpsertTrace(t *Trace) error {
	_, err := s.db.Exec(`
		INSERT INTO traces (id, conversation_id, ts, source, status, workspace, project, prompt, files_modified, loop_count, raw_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ts = excluded.ts, status = excluded.status, prompt = excluded.prompt,
			files_modified = excluded.files_modified, loop_count = excluded.loop_count
	`, t.ID, t.ConversationID, t.TS, t.Source, t.Status, t.Workspace, t.Project, t.Prompt, t.FilesModified, t.LoopCount, t.RawPath)
	if err != nil {
		return fmt.Errorf("upsert trace %q: %w", t.ID, err)
	}
	return nil
}

func (s *Store) GetTrace(id string) (*Trace, error) {
	row := s.db.QueryRow("SELECT id, conversation_id, ts, source, status, workspace, project, prompt, files_modified, loop_count, raw_path FROM traces WHERE id = ?", id)
	var t Trace
	var convID, ts, source, status, workspace, project, prompt, files, rawPath sql.NullString
	var loopCount sql.NullInt64
	if err := row.Scan(&t.ID, &convID, &ts, &source, &status, &workspace, &project, &prompt, &files, &loopCount, &rawPath); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get trace %q: %w", id, err)
	}
	t.ConversationID = convID.String
	t.TS = ts.String
	t.Source = source.String
	t.Status = status.String
	t.Workspace = workspace.String
	t.Project = project.String
	t.Prompt = prompt.String
	t.FilesModified = files.String
	t.LoopCount = int(loopCount.Int64)
	t.RawPath = rawPath.String
	return &t, nil
}

func (s *Store) ListTraces() ([]Trace, error) {
	rows, err := s.db.Query("SELECT id, conversation_id, ts, source, status, workspace, project, prompt, files_modified, loop_count, raw_path FROM traces ORDER BY ts DESC")
	if err != nil {
		return nil, fmt.Errorf("list traces: %w", err)
	}
	defer rows.Close()
	return scanTraces(rows)
}

func (s *Store) DeleteTrace(id string) error {
	// Manual cascade: delete summaries, embeddings, then trace
	for _, q := range []string{
		"DELETE FROM embeddings WHERE trace_id = ?",
		"DELETE FROM summaries WHERE trace_id = ?",
		"DELETE FROM traces WHERE id = ?",
	} {
		if _, err := s.db.Exec(q, id); err != nil {
			return fmt.Errorf("delete trace %q: %w", id, err)
		}
	}
	return nil
}

func scanTraces(rows *sql.Rows) ([]Trace, error) {
	var traces []Trace
	for rows.Next() {
		var t Trace
		var convID, ts, source, status, workspace, project, prompt, files, rawPath sql.NullString
		var loopCount sql.NullInt64
		if err := rows.Scan(&t.ID, &convID, &ts, &source, &status, &workspace, &project, &prompt, &files, &loopCount, &rawPath); err != nil {
			return nil, fmt.Errorf("scan trace: %w", err)
		}
		t.ConversationID = convID.String
		t.TS = ts.String
		t.Source = source.String
		t.Status = status.String
		t.Workspace = workspace.String
		t.Project = project.String
		t.Prompt = prompt.String
		t.FilesModified = files.String
		t.LoopCount = int(loopCount.Int64)
		t.RawPath = rawPath.String
		traces = append(traces, t)
	}
	return traces, rows.Err()
}

// ── Summary types and methods ────────────────────────────────────────────────

type Summary struct {
	TraceID         string
	Problem         string
	Pattern         string
	Skills          string // JSON array
	Pitfalls        string
	EmbeddingText   string
	ProjectTag      string
	Component       string
	DomainKnowledge string
	SummarizedAt    string
}

func (s *Store) UpsertSummary(sm *Summary) error {
	_, err := s.db.Exec(`
		INSERT INTO summaries (trace_id, problem, pattern, skills, pitfalls, embedding_text, project_tag, component, domain_knowledge, summarized_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id) DO UPDATE SET
			problem = excluded.problem, pattern = excluded.pattern, skills = excluded.skills,
			pitfalls = excluded.pitfalls, embedding_text = excluded.embedding_text,
			project_tag = excluded.project_tag, component = excluded.component,
			domain_knowledge = excluded.domain_knowledge, summarized_at = excluded.summarized_at
	`, sm.TraceID, sm.Problem, sm.Pattern, sm.Skills, sm.Pitfalls, sm.EmbeddingText, sm.ProjectTag, sm.Component, sm.DomainKnowledge, sm.SummarizedAt)
	if err != nil {
		return fmt.Errorf("upsert summary: %w", err)
	}
	return nil
}

func (s *Store) GetSummary(traceID string) (*Summary, error) {
	row := s.db.QueryRow("SELECT trace_id, problem, pattern, skills, pitfalls, embedding_text, project_tag, component, domain_knowledge, summarized_at FROM summaries WHERE trace_id = ?", traceID)
	var sm Summary
	if err := row.Scan(&sm.TraceID, &sm.Problem, &sm.Pattern, &sm.Skills, &sm.Pitfalls, &sm.EmbeddingText, &sm.ProjectTag, &sm.Component, &sm.DomainKnowledge, &sm.SummarizedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get summary: %w", err)
	}
	return &sm, nil
}

func (s *Store) UnsummarizedTraces() ([]Trace, error) {
	rows, err := s.db.Query("SELECT t.id, t.conversation_id, t.ts, t.source, t.status, t.workspace, t.project, t.prompt, t.files_modified, t.loop_count, t.raw_path FROM traces t LEFT JOIN summaries s ON t.id = s.trace_id WHERE s.trace_id IS NULL ORDER BY t.ts")
	if err != nil {
		return nil, fmt.Errorf("unsummarized traces: %w", err)
	}
	defer rows.Close()
	return scanTraces(rows)
}

func (s *Store) AllSummaries() ([]Summary, error) {
	rows, err := s.db.Query("SELECT trace_id, problem, pattern, skills, pitfalls, embedding_text, project_tag, component, domain_knowledge, summarized_at FROM summaries ORDER BY summarized_at")
	if err != nil {
		return nil, fmt.Errorf("all summaries: %w", err)
	}
	defer rows.Close()
	var summaries []Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.TraceID, &sm.Problem, &sm.Pattern, &sm.Skills, &sm.Pitfalls, &sm.EmbeddingText, &sm.ProjectTag, &sm.Component, &sm.DomainKnowledge, &sm.SummarizedAt); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		summaries = append(summaries, sm)
	}
	return summaries, rows.Err()
}

func (s *Store) AllProjectTags() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT project_tag FROM summaries WHERE project_tag != '' ORDER BY project_tag")
	if err != nil {
		return nil, fmt.Errorf("all project tags: %w", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// ── Embedding methods ────────────────────────────────────────────────────────

func (s *Store) UpsertEmbedding(traceID string, vector []byte) error {
	_, err := s.db.Exec(`
		INSERT INTO embeddings (trace_id, vector) VALUES (?, ?)
		ON CONFLICT(trace_id) DO UPDATE SET vector = excluded.vector
	`, traceID, vector)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

func (s *Store) UnembeddedSummaries() ([]Summary, error) {
	rows, err := s.db.Query("SELECT s.trace_id, s.problem, s.pattern, s.skills, s.pitfalls, s.embedding_text, s.project_tag, s.component, s.domain_knowledge, s.summarized_at FROM summaries s LEFT JOIN embeddings e ON s.trace_id = e.trace_id WHERE e.trace_id IS NULL")
	if err != nil {
		return nil, fmt.Errorf("unembedded summaries: %w", err)
	}
	defer rows.Close()
	var summaries []Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.TraceID, &sm.Problem, &sm.Pattern, &sm.Skills, &sm.Pitfalls, &sm.EmbeddingText, &sm.ProjectTag, &sm.Component, &sm.DomainKnowledge, &sm.SummarizedAt); err != nil {
			return nil, err
		}
		summaries = append(summaries, sm)
	}
	return summaries, rows.Err()
}

type EmbeddingRow struct {
	TraceID string
	Vector  []byte
}

func (s *Store) AllEmbeddings() ([]EmbeddingRow, error) {
	rows, err := s.db.Query("SELECT trace_id, vector FROM embeddings")
	if err != nil {
		return nil, fmt.Errorf("all embeddings: %w", err)
	}
	defer rows.Close()
	var result []EmbeddingRow
	for rows.Next() {
		var r EmbeddingRow
		if err := rows.Scan(&r.TraceID, &r.Vector); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ── Candidate methods ────────────────────────────────────────────────────────

type Candidate struct {
	ID           string
	Type         string
	Slug         string
	Definition   string
	Status       string
	Frequency    int
	SourceTraces string // JSON array
	DetectedAt   string
	ResolvedAt   string
}

func (s *Store) UpsertCandidate(c *Candidate) error {
	_, err := s.db.Exec(`
		INSERT INTO candidates (id, type, slug, definition, status, frequency, source_traces, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			definition = excluded.definition, frequency = excluded.frequency,
			source_traces = excluded.source_traces, detected_at = excluded.detected_at
	`, c.ID, c.Type, c.Slug, c.Definition, c.Status, c.Frequency, c.SourceTraces, c.DetectedAt)
	if err != nil {
		return fmt.Errorf("upsert candidate: %w", err)
	}
	return nil
}

func (s *Store) ListCandidates(status string) ([]Candidate, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.Query("SELECT id, type, slug, definition, status, frequency, source_traces, detected_at, resolved_at FROM candidates ORDER BY detected_at DESC")
	} else {
		rows, err = s.db.Query("SELECT id, type, slug, definition, status, frequency, source_traces, detected_at, resolved_at FROM candidates WHERE status = ? ORDER BY detected_at DESC", status)
	}
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()
	var candidates []Candidate
	for rows.Next() {
		var c Candidate
		var resolvedAt sql.NullString
		if err := rows.Scan(&c.ID, &c.Type, &c.Slug, &c.Definition, &c.Status, &c.Frequency, &c.SourceTraces, &c.DetectedAt, &resolvedAt); err != nil {
			return nil, err
		}
		c.ResolvedAt = resolvedAt.String
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (s *Store) ApproveCandidate(id string) error {
	_, err := s.db.Exec("UPDATE candidates SET status = 'approved', resolved_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?", id)
	return err
}

func (s *Store) DismissCandidate(id string) error {
	_, err := s.db.Exec("UPDATE candidates SET status = 'dismissed', resolved_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?", id)
	return err
}

func (s *Store) ExistingCandidateSlugs() (map[string]bool, error) {
	rows, err := s.db.Query("SELECT slug FROM candidates")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slugs := make(map[string]bool)
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs[slug] = true
	}
	return slugs, rows.Err()
}

// ── Knowledge methods ────────────────────────────────────────────────────────

type KnowledgeItem struct {
	ID        string
	Namespace string
	Slug      string
	FilePath  string
	Summary   string
	Tags      string // JSON array
	Source    string
	Frequency int
	PromotedAt string
	Vector    []byte
}

func (s *Store) UpsertKnowledge(k *KnowledgeItem) error {
	_, err := s.db.Exec(`
		INSERT INTO knowledge (id, namespace, slug, file_path, summary, tags, source, frequency, promoted_at, vector)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			summary = excluded.summary, tags = excluded.tags, frequency = excluded.frequency,
			promoted_at = excluded.promoted_at, vector = excluded.vector
	`, k.ID, k.Namespace, k.Slug, k.FilePath, k.Summary, k.Tags, k.Source, k.Frequency, k.PromotedAt, k.Vector)
	if err != nil {
		return fmt.Errorf("upsert knowledge: %w", err)
	}
	return nil
}

func (s *Store) DeleteKnowledge(id string) error {
	_, err := s.db.Exec("DELETE FROM knowledge WHERE id = ?", id)
	return err
}

func (s *Store) ListKnowledge() ([]KnowledgeItem, error) {
	rows, err := s.db.Query("SELECT id, namespace, slug, file_path, summary, tags, source, frequency, promoted_at FROM knowledge ORDER BY promoted_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list knowledge: %w", err)
	}
	defer rows.Close()
	var items []KnowledgeItem
	for rows.Next() {
		var k KnowledgeItem
		if err := rows.Scan(&k.ID, &k.Namespace, &k.Slug, &k.FilePath, &k.Summary, &k.Tags, &k.Source, &k.Frequency, &k.PromotedAt); err != nil {
			return nil, err
		}
		items = append(items, k)
	}
	return items, rows.Err()
}

// ── Chunk methods ────────────────────────────────────────────────────────────

type Chunk struct {
	ID         string
	SourceID   string
	SourcePath string
	Namespace  string
	ChunkIndex int
	Heading    string
	Content    string
	Vector     []byte
}

func (s *Store) UpsertChunk(c *Chunk) error {
	_, err := s.db.Exec(`
		INSERT INTO chunks (id, source_id, source_path, namespace, chunk_index, heading, content, vector)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			content = excluded.content, heading = excluded.heading, vector = excluded.vector
	`, c.ID, c.SourceID, c.SourcePath, c.Namespace, c.ChunkIndex, c.Heading, c.Content, c.Vector)
	if err != nil {
		return fmt.Errorf("upsert chunk: %w", err)
	}
	return nil
}

func (s *Store) DeleteChunksBySource(sourceID string) error {
	_, err := s.db.Exec("DELETE FROM chunks WHERE source_id = ?", sourceID)
	return err
}

func (s *Store) AllChunks() ([]Chunk, error) {
	rows, err := s.db.Query("SELECT id, source_id, source_path, namespace, chunk_index, heading, content, vector FROM chunks")
	if err != nil {
		return nil, fmt.Errorf("all chunks: %w", err)
	}
	defer rows.Close()
	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.ID, &c.SourceID, &c.SourcePath, &c.Namespace, &c.ChunkIndex, &c.Heading, &c.Content, &c.Vector); err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

type ChunkSource struct {
	SourceID   string
	SourcePath string
}

func (s *Store) DistinctChunkSources() ([]ChunkSource, error) {
	rows, err := s.db.Query("SELECT DISTINCT source_id, source_path FROM chunks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []ChunkSource
	for rows.Next() {
		var cs ChunkSource
		if err := rows.Scan(&cs.SourceID, &cs.SourcePath); err != nil {
			return nil, err
		}
		sources = append(sources, cs)
	}
	return sources, rows.Err()
}

// ── Usage methods ────────────────────────────────────────────────────────────

type UsageRecord struct {
	ID           int
	TS           string
	Task         string
	Model        string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	TraceID      string
}

func (s *Store) LogUsage(u *UsageRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO usage (ts, task, model, input_tokens, output_tokens, cost_usd, trace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, u.TS, u.Task, u.Model, u.InputTokens, u.OutputTokens, u.CostUSD, u.TraceID)
	if err != nil {
		return fmt.Errorf("log usage: %w", err)
	}
	return nil
}

func (s *Store) UsageByTask() (map[string]float64, error) {
	rows, err := s.db.Query("SELECT task, SUM(cost_usd) FROM usage GROUP BY task")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]float64)
	for rows.Next() {
		var task string
		var cost float64
		if err := rows.Scan(&task, &cost); err != nil {
			return nil, err
		}
		result[task] = cost
	}
	return result, rows.Err()
}

func (s *Store) UsageByDay() ([]UsageRecord, error) {
	rows, err := s.db.Query("SELECT 0, substr(ts, 1, 10), '', '', SUM(input_tokens), SUM(output_tokens), SUM(cost_usd), '' FROM usage GROUP BY substr(ts, 1, 10) ORDER BY substr(ts, 1, 10) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(&r.ID, &r.TS, &r.Task, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.TraceID); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) RecentUsage(limit int) ([]UsageRecord, error) {
	rows, err := s.db.Query("SELECT id, ts, task, model, input_tokens, output_tokens, cost_usd, COALESCE(trace_id, '') FROM usage ORDER BY ts DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []UsageRecord
	for rows.Next() {
		var r UsageRecord
		if err := rows.Scan(&r.ID, &r.TS, &r.Task, &r.Model, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.TraceID); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// ── Meta methods ─────────────────────────────────────────────────────────────

func (s *Store) GetMeta(key string) (string, error) {
	row := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key)
	var val string
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// ── Synthesis History ────────────────────────────────────────────────────

func (s *Store) LogSynthesis(id, skillPath string, version int, sourcesJSON, feedback, model string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`
		INSERT INTO synthesis_history (id, skill_path, version, sources, feedback, synthesized_at, model)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, skillPath, version, sourcesJSON, feedback, now, model)
	if err != nil {
		return fmt.Errorf("log synthesis: %w", err)
	}
	return nil
}

// ── Stats ────────────────────────────────────────────────────────────────────

func (s *Store) Stats() (map[string]int, error) {
	tables := []string{"entries", "traces", "summaries", "embeddings", "candidates", "knowledge", "chunks", "usage"}
	result := make(map[string]int)
	for _, t := range tables {
		row := s.db.QueryRow("SELECT COUNT(*) FROM " + t)
		var count int
		if err := row.Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		result[t] = count
	}
	return result, nil
}
