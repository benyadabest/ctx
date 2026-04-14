package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
)

// IndexResult holds stats from an embed/index run.
type IndexResult struct {
	TracesTotal     int
	TracesEmbedded  int
	FilesTotal      int
	FilesEmbedded   int
	ChunksCreated   int
	StalesPruned    int
}

// IndexAll embeds unembedded trace summaries and namespace files.
// If forceAll is true, re-embeds everything.
func IndexAll(ctx context.Context, s *store.Store, embedder *vectors.Embedder, cfg *config.Config, forceAll bool) (*IndexResult, error) {
	result := &IndexResult{}

	// Phase 1: Trace summaries → embeddings table
	var summaries []store.Summary
	var err error
	if forceAll {
		summaries, err = s.AllSummaries()
	} else {
		summaries, err = s.UnembeddedSummaries()
	}
	if err != nil {
		return nil, fmt.Errorf("get summaries: %w", err)
	}

	result.TracesTotal = len(summaries)
	for _, sm := range summaries {
		if sm.EmbeddingText == "" {
			continue
		}
		vec, err := embedder.Embed(ctx, sm.EmbeddingText)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: embed failed: %v\n", sm.TraceID, err)
			continue
		}
		if err := s.UpsertEmbedding(sm.TraceID, vectors.Pack(vec)); err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: store failed: %v\n", sm.TraceID, err)
			continue
		}
		result.TracesEmbedded++
	}

	// Phase 2: Namespace files → chunks table
	nsItems := scanNamespaceFiles(cfg.ContextDir())
	if !forceAll {
		existing, _ := existingChunkSources(s)
		var filtered []nsFile
		for _, it := range nsItems {
			if !existing[it.sourceID] {
				filtered = append(filtered, it)
			}
		}
		nsItems = filtered
	}

	result.FilesTotal = len(nsItems)
	for _, it := range nsItems {
		n, err := embedNamespaceFile(ctx, s, embedder, cfg, it)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: %v\n", it.sourceID, err)
			continue
		}
		if n > 0 {
			result.FilesEmbedded++
			result.ChunksCreated += n
		}
	}

	// Phase 3: Prune stale chunks for deleted files
	result.StalesPruned = pruneStaleChunks(s, cfg.ContextDir())

	// Update meta
	s.SetMeta("last_embed_ts", time.Now().UTC().Format(time.RFC3339))

	return result, nil
}

// IndexTrace embeds a single trace summary.
func IndexTrace(ctx context.Context, s *store.Store, embedder *vectors.Embedder, traceID string) error {
	sm, err := s.GetSummary(traceID)
	if err != nil {
		return err
	}
	if sm == nil {
		return fmt.Errorf("no summary found for %s", traceID)
	}
	if sm.EmbeddingText == "" {
		return fmt.Errorf("empty embedding_text for %s", traceID)
	}

	vec, err := embedder.Embed(ctx, sm.EmbeddingText)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}
	return s.UpsertEmbedding(traceID, vectors.Pack(vec))
}

// ── Internal helpers ────────────────────────────────────────────────────────

type nsFile struct {
	sourceID  string
	path      string
	namespace string
}

func scanNamespaceFiles(contextDir string) []nsFile {
	scanDirs := map[string]string{
		"docs":            "docs",
		"plans":           "plans",
		"patterns":        "patterns",
		"debugging":       "debugging",
		"skills/internal": "skills/internal",
		"skills/external": "skills/external",
		"primitives":      "primitives",
	}

	var items []nsFile
	for subdir, namespace := range scanDirs {
		dirPath := filepath.Join(contextDir, subdir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			stem := strings.TrimSuffix(entry.Name(), ".md")
			sourceID := "ns-" + strings.ReplaceAll(namespace, "/", "-") + "-" + stem
			items = append(items, nsFile{
				sourceID:  sourceID,
				path:      filepath.Join(dirPath, entry.Name()),
				namespace: namespace,
			})
		}
	}
	return items
}

func existingChunkSources(s *store.Store) (map[string]bool, error) {
	sources, err := s.DistinctChunkSources()
	if err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(sources))
	for _, cs := range sources {
		m[cs.SourceID] = true
	}
	return m, nil
}

func embedNamespaceFile(ctx context.Context, s *store.Store, embedder *vectors.Embedder, cfg *config.Config, item nsFile) (int, error) {
	content, err := os.ReadFile(item.path)
	if err != nil {
		return 0, err
	}

	relPath, _ := filepath.Rel(cfg.ContextDir(), item.path)
	text := string(content)

	if NeedsChunking(item.namespace) && EstimateTokens(text) > 300 {
		chunks := ChunkMarkdown(text, 400, 50)
		// Clear old chunks
		s.DeleteChunksBySource(item.sourceID)

		created := 0
		for i, chunk := range chunks {
			chunkID := fmt.Sprintf("%s-chunk-%d", item.sourceID, i)
			vec, err := embedder.Embed(ctx, chunk.Content)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  SKIP chunk %s: %v\n", chunkID, err)
				continue
			}
			err = s.UpsertChunk(&store.Chunk{
				ID:         chunkID,
				SourceID:   item.sourceID,
				SourcePath: relPath,
				Namespace:  item.namespace,
				ChunkIndex: i,
				Heading:    chunk.Heading,
				Content:    chunk.Content,
				Vector:     vectors.Pack(vec),
			})
			if err != nil {
				continue
			}
			created++
		}
		return created, nil
	}

	// Short doc — single chunk
	body := text
	if strings.HasPrefix(text, "---") {
		parts := strings.SplitN(text, "---", 3)
		if len(parts) >= 3 {
			body = strings.TrimSpace(parts[2])
		}
	}
	if strings.TrimSpace(body) == "" {
		return 0, nil
	}
	if len(body) > 1500 {
		body = body[:1500]
	}

	vec, err := embedder.Embed(ctx, body)
	if err != nil {
		return 0, err
	}

	s.DeleteChunksBySource(item.sourceID)
	err = s.UpsertChunk(&store.Chunk{
		ID:         item.sourceID + "-chunk-0",
		SourceID:   item.sourceID,
		SourcePath: relPath,
		Namespace:  item.namespace,
		ChunkIndex: 0,
		Heading:    "(full)",
		Content:    body,
		Vector:     vectors.Pack(vec),
	})
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func pruneStaleChunks(s *store.Store, contextDir string) int {
	sources, err := s.DistinctChunkSources()
	if err != nil {
		return 0
	}
	pruned := 0
	for _, cs := range sources {
		if cs.SourcePath == "" {
			continue
		}
		fullPath := filepath.Join(contextDir, cs.SourcePath)
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			s.DeleteChunksBySource(cs.SourceID)
			pruned++
		}
	}
	return pruned
}
