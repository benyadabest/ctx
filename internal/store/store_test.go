package store

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyValidation(t *testing.T) {
	valid := []string{"a", "foo", "react.hooks", "my-key.sub-key", "a1.b2.c3"}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}

	invalid := []string{"", "A", "foo.Bar", "foo..bar", ".foo", "foo.", "foo bar", "foo/bar", "foo_bar"}
	for _, k := range invalid {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want error", k)
		}
	}
}

func TestSetAndGet(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("test.key", "hello", "text/plain"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	e, err := s.Get("test.key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e == nil {
		t.Fatal("Get returned nil")
	}
	if e.Value != "hello" {
		t.Errorf("Value = %q, want %q", e.Value, "hello")
	}
	if e.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", e.ContentType, "text/plain")
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)

	e, err := s.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e != nil {
		t.Errorf("Get returned %v, want nil", e)
	}
}

func TestUpsertPreservesCreatedAt(t *testing.T) {
	s := newTestStore(t)

	s.Set("test.key", "v1", "text/plain")
	e1, _ := s.Get("test.key")

	s.Set("test.key", "v2", "text/plain")
	e2, _ := s.Get("test.key")

	if e2.Value != "v2" {
		t.Errorf("Value = %q, want %q", e2.Value, "v2")
	}
	if e2.CreatedAt != e1.CreatedAt {
		t.Errorf("CreatedAt changed from %q to %q", e1.CreatedAt, e2.CreatedAt)
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)

	s.Set("test.key", "hello", "text/plain")
	if err := s.Delete("test.key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	e, _ := s.Get("test.key")
	if e != nil {
		t.Error("entry still exists after delete")
	}
}

func TestDeleteNonexistent(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete("nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)

	s.Set("react.hooks", "hooks info", "text/plain")
	s.Set("react.components", "components info", "text/plain")
	s.Set("go.errors", "errors info", "text/plain")

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List all returned %d entries, want 3", len(all))
	}

	react, err := s.List("react")
	if err != nil {
		t.Fatalf("List react: %v", err)
	}
	if len(react) != 2 {
		t.Errorf("List react returned %d entries, want 2", len(react))
	}

	goEntries, err := s.List("go")
	if err != nil {
		t.Fatalf("List go: %v", err)
	}
	if len(goEntries) != 1 {
		t.Errorf("List go returned %d entries, want 1", len(goEntries))
	}
}

func TestListExactMatch(t *testing.T) {
	s := newTestStore(t)

	s.Set("react", "top level", "text/plain")
	s.Set("react.hooks", "hooks", "text/plain")

	entries, err := s.List("react")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("List returned %d entries, want 2", len(entries))
	}
}

func TestSearch(t *testing.T) {
	s := newTestStore(t)

	s.Set("react.hooks", "Always call hooks at the top level of your component", "text/plain")
	s.Set("go.errors", "Wrap errors with fmt.Errorf and %w", "text/plain")

	results, err := s.Search("hooks")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("Search returned %d results, want 1", len(results))
	}
	if len(results) > 0 && results[0].Key != "react.hooks" {
		t.Errorf("Search result key = %q, want %q", results[0].Key, "react.hooks")
	}
}

func TestSearchAfterUpdate(t *testing.T) {
	s := newTestStore(t)

	s.Set("test.key", "original content about hooks", "text/plain")
	s.Set("test.key", "updated content about errors", "text/plain")

	hooks, _ := s.Search("hooks")
	if len(hooks) != 0 {
		t.Errorf("Search 'hooks' returned %d results after update, want 0", len(hooks))
	}

	errors, _ := s.Search("errors")
	if len(errors) != 1 {
		t.Errorf("Search 'errors' returned %d results after update, want 1", len(errors))
	}
}

func TestSearchAfterDelete(t *testing.T) {
	s := newTestStore(t)

	s.Set("test.key", "searchable content", "text/plain")
	s.Delete("test.key")

	results, _ := s.Search("searchable")
	if len(results) != 0 {
		t.Errorf("Search returned %d results after delete, want 0", len(results))
	}
}

func TestNewCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "dir", "test.db")

	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Close()

	if _, err := os.Stat(filepath.Join(dir, "sub", "dir")); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}

// ── Trace tests ──────────────────────────────────────────────────────────────

func TestTraceUpsertAndGet(t *testing.T) {
	s := newTestStore(t)

	tr := &Trace{
		ID: "trace-abc123", ConversationID: "conv-1", TS: "2026-01-01T00:00:00Z",
		Source: "claude-code", Status: "completed", Project: "ctx", Prompt: "build CLI",
		FilesModified: `["main.go"]`, LoopCount: 3,
	}
	if err := s.UpsertTrace(tr); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}

	got, err := s.GetTrace("trace-abc123")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if got == nil {
		t.Fatal("GetTrace returned nil")
	}
	if got.Source != "claude-code" || got.Project != "ctx" || got.LoopCount != 3 {
		t.Errorf("GetTrace fields mismatch: %+v", got)
	}
}

func TestTraceUpsertUpdates(t *testing.T) {
	s := newTestStore(t)

	s.UpsertTrace(&Trace{ID: "trace-1", Status: "running", Prompt: "v1"})
	s.UpsertTrace(&Trace{ID: "trace-1", Status: "completed", Prompt: "v2"})

	got, _ := s.GetTrace("trace-1")
	if got.Status != "completed" || got.Prompt != "v2" {
		t.Errorf("upsert did not update: %+v", got)
	}
}

func TestListTraces(t *testing.T) {
	s := newTestStore(t)

	s.UpsertTrace(&Trace{ID: "trace-1", TS: "2026-01-01T00:00:00Z"})
	s.UpsertTrace(&Trace{ID: "trace-2", TS: "2026-01-02T00:00:00Z"})

	traces, err := s.ListTraces()
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(traces) != 2 {
		t.Errorf("ListTraces returned %d, want 2", len(traces))
	}
}

func TestDeleteTraceCascades(t *testing.T) {
	s := newTestStore(t)

	s.UpsertTrace(&Trace{ID: "trace-1"})
	s.UpsertSummary(&Summary{TraceID: "trace-1", Problem: "test"})
	s.UpsertEmbedding("trace-1", []byte{1, 2, 3})

	if err := s.DeleteTrace("trace-1"); err != nil {
		t.Fatalf("DeleteTrace: %v", err)
	}

	tr, _ := s.GetTrace("trace-1")
	if tr != nil {
		t.Error("trace still exists after delete")
	}
	sm, _ := s.GetSummary("trace-1")
	if sm != nil {
		t.Error("summary still exists after trace delete")
	}
}

// ── Summary tests ────────────────────────────────────────────────────────────

func TestSummaryUpsertAndGet(t *testing.T) {
	s := newTestStore(t)

	s.UpsertTrace(&Trace{ID: "trace-1"})
	sm := &Summary{
		TraceID: "trace-1", Problem: "auth broke", Pattern: "token refresh",
		Skills: `["auth","jwt"]`, Pitfalls: "silent expiry", EmbeddingText: "auth token refresh pattern",
		ProjectTag: "api", SummarizedAt: "2026-01-01T00:00:00Z",
	}
	if err := s.UpsertSummary(sm); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	got, err := s.GetSummary("trace-1")
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got.Problem != "auth broke" || got.ProjectTag != "api" {
		t.Errorf("GetSummary mismatch: %+v", got)
	}
}

func TestUnsummarizedTraces(t *testing.T) {
	s := newTestStore(t)

	s.UpsertTrace(&Trace{ID: "trace-1", TS: "2026-01-01T00:00:00Z"})
	s.UpsertTrace(&Trace{ID: "trace-2", TS: "2026-01-02T00:00:00Z"})
	s.UpsertSummary(&Summary{TraceID: "trace-1", SummarizedAt: "2026-01-01T00:00:00Z"})

	unsummarized, err := s.UnsummarizedTraces()
	if err != nil {
		t.Fatalf("UnsummarizedTraces: %v", err)
	}
	if len(unsummarized) != 1 || unsummarized[0].ID != "trace-2" {
		t.Errorf("UnsummarizedTraces: got %v", unsummarized)
	}
}

// ── Candidate tests ──────────────────────────────────────────────────────────

func TestCandidateLifecycle(t *testing.T) {
	s := newTestStore(t)

	c := &Candidate{
		ID: "cand-1", Type: "skill", Slug: "auth-patterns", Definition: "how to auth",
		Status: "pending", Frequency: 4, SourceTraces: `["trace-1","trace-2"]`,
		DetectedAt: "2026-01-01T00:00:00Z",
	}
	if err := s.UpsertCandidate(c); err != nil {
		t.Fatalf("UpsertCandidate: %v", err)
	}

	pending, _ := s.ListCandidates("pending")
	if len(pending) != 1 {
		t.Fatalf("ListCandidates pending: got %d, want 1", len(pending))
	}

	s.ApproveCandidate("cand-1")
	approved, _ := s.ListCandidates("approved")
	if len(approved) != 1 || approved[0].ResolvedAt == "" {
		t.Errorf("ApproveCandidate failed: %+v", approved)
	}
}

// ── Meta tests ───────────────────────────────────────────────────────────────

func TestMeta(t *testing.T) {
	s := newTestStore(t)

	s.SetMeta("last_run", "2026-01-01")
	val, err := s.GetMeta("last_run")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "2026-01-01" {
		t.Errorf("GetMeta = %q, want %q", val, "2026-01-01")
	}

	s.SetMeta("last_run", "2026-01-02")
	val, _ = s.GetMeta("last_run")
	if val != "2026-01-02" {
		t.Errorf("SetMeta upsert failed: %q", val)
	}
}

func TestGetMetaNotFound(t *testing.T) {
	s := newTestStore(t)
	val, err := s.GetMeta("nonexistent")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta nonexistent = %q, want empty", val)
	}
}

// ── Usage tests ──────────────────────────────────────────────────────────────

func TestUsageLog(t *testing.T) {
	s := newTestStore(t)

	s.LogUsage(&UsageRecord{TS: "2026-01-01T00:00:00Z", Task: "summarize", Model: "ollama/mistral", InputTokens: 100, OutputTokens: 50, CostUSD: 0.0})
	s.LogUsage(&UsageRecord{TS: "2026-01-01T01:00:00Z", Task: "compile", Model: "claude-sonnet", InputTokens: 500, OutputTokens: 200, CostUSD: 0.01})

	recent, err := s.RecentUsage(10)
	if err != nil {
		t.Fatalf("RecentUsage: %v", err)
	}
	if len(recent) != 2 {
		t.Errorf("RecentUsage: got %d, want 2", len(recent))
	}

	byTask, err := s.UsageByTask()
	if err != nil {
		t.Fatalf("UsageByTask: %v", err)
	}
	if len(byTask) != 2 {
		t.Errorf("UsageByTask: got %d tasks, want 2", len(byTask))
	}
}

// ── Stats test ───────────────────────────────────────────────────────────────

func TestStats(t *testing.T) {
	s := newTestStore(t)

	s.Set("test.key", "val", "text/plain")
	s.UpsertTrace(&Trace{ID: "trace-1"})

	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats["entries"] != 1 || stats["traces"] != 1 {
		t.Errorf("Stats mismatch: %v", stats)
	}
}

// ── Chunk tests ──────────────────────────────────────────────────────────────

func TestChunkLifecycle(t *testing.T) {
	s := newTestStore(t)

	c := &Chunk{
		ID: "chunk-1", SourceID: "doc-1", SourcePath: "docs/api.md",
		Namespace: "docs", ChunkIndex: 0, Heading: "Overview",
		Content: "API overview text", Vector: []byte{1, 2, 3},
	}
	if err := s.UpsertChunk(c); err != nil {
		t.Fatalf("UpsertChunk: %v", err)
	}

	chunks, err := s.AllChunks()
	if err != nil {
		t.Fatalf("AllChunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Heading != "Overview" {
		t.Errorf("AllChunks mismatch: %v", chunks)
	}

	if err := s.DeleteChunksBySource("doc-1"); err != nil {
		t.Fatalf("DeleteChunksBySource: %v", err)
	}

	chunks, _ = s.AllChunks()
	if len(chunks) != 0 {
		t.Error("chunks still exist after delete by source")
	}
}
