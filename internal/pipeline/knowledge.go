package pipeline

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
)

// ── Unified Key ↔ Path ─────────────────────────────────────────────────────

// KeyToPath converts a dot-separated key to a relative file path.
// "docs.api-guide" → "docs/api-guide.md"
// "skills.internal.retry" → "skills/internal/retry.md"
func KeyToPath(key string) string {
	return strings.ReplaceAll(key, ".", string(filepath.Separator)) + ".md"
}

// PathToKey converts a relative .md path to a dot-separated key.
// "docs/api-guide.md" → "docs.api-guide"
func PathToKey(relPath string) string {
	p := strings.TrimSuffix(relPath, ".md")
	return strings.ReplaceAll(p, string(filepath.Separator), ".")
}

// SetKey writes content to a namespace file and indexes it everywhere:
//   - File on disk at ~/.context/<key-as-path>.md
//   - entries table (for FTS search)
//   - knowledge table (for web UI + compile)
//   - chunks table (if embedder provided)
func SetKey(ctx context.Context, s *store.Store, embedder *vectors.Embedder, cfg *config.Config, key, value, contentType string) error {
	relPath := KeyToPath(key)
	fullPath := filepath.Join(cfg.ContextDir(), relPath)

	// Ensure parent dir
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// Write the file
	if err := os.WriteFile(fullPath, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// Index in entries table for FTS
	s.Set(key, value, contentType)

	// Derive namespace + slug from key parts
	parts := strings.Split(key, ".")
	namespace := parts[0]
	if len(parts) > 2 {
		namespace = strings.Join(parts[:len(parts)-1], "/")
	}
	slug := parts[len(parts)-1]

	sourceID := "ns-" + strings.ReplaceAll(strings.TrimSuffix(relPath, ".md"), string(filepath.Separator), "-")

	s.UpsertKnowledge(&store.KnowledgeItem{
		ID:         sourceID,
		Namespace:  namespace,
		Slug:       slug,
		FilePath:   fullPath,
		Summary:    truncate(value, 200),
		Tags:       "[]",
		Source:     "set",
		Frequency:  1,
		PromotedAt: time.Now().UTC().Format(time.RFC3339),
	})

	// Chunk + embed if available
	if embedder != nil {
		chunks := ChunkMarkdown(value, 400, 50)
		s.DeleteChunksBySource(sourceID)
		for i, chunk := range chunks {
			chunkID := fmt.Sprintf("%s-chunk-%d", sourceID, i)
			vec, err := embedder.Embed(ctx, chunk.Content)
			if err != nil {
				continue
			}
			s.UpsertChunk(&store.Chunk{
				ID:         chunkID,
				SourceID:   sourceID,
				SourcePath: relPath,
				Namespace:  namespace,
				ChunkIndex: i,
				Heading:    chunk.Heading,
				Content:    chunk.Content,
				Vector:     vectors.Pack(vec),
			})
		}
	}

	return nil
}

// GetKey reads content from a namespace file, falling back to the entries table.
func GetKey(s *store.Store, cfg *config.Config, key string) (string, string, error) {
	relPath := KeyToPath(key)
	fullPath := filepath.Join(cfg.ContextDir(), relPath)

	if data, err := os.ReadFile(fullPath); err == nil {
		return string(data), "text/markdown", nil
	}

	// Fallback to entries table
	e, err := s.Get(key)
	if err != nil {
		return "", "", err
	}
	if e == nil {
		return "", "", fmt.Errorf("not found: %s", key)
	}
	return e.Value, e.ContentType, nil
}

// DeleteKey removes a namespace file and all associated DB entries.
func DeleteKey(s *store.Store, cfg *config.Config, key string) error {
	relPath := KeyToPath(key)
	fullPath := filepath.Join(cfg.ContextDir(), relPath)

	// Remove file (ignore if missing)
	os.Remove(fullPath)

	// Remove from entries table
	s.Delete(key)

	// Remove from knowledge + chunks
	sourceID := "ns-" + strings.ReplaceAll(strings.TrimSuffix(relPath, ".md"), string(filepath.Separator), "-")
	s.DeleteChunksBySource(sourceID)
	s.DeleteKnowledge(sourceID)

	return nil
}

// ListKeys scans the context directory and merges with entries table keys.
func ListKeys(s *store.Store, cfg *config.Config, prefix string) ([]string, error) {
	seen := make(map[string]bool)
	var keys []string

	// Walk filesystem
	contextDir := cfg.ContextDir()
	filepath.Walk(contextDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(contextDir, path)
		key := PathToKey(rel)
		if prefix == "" || key == prefix || strings.HasPrefix(key, prefix+".") {
			keys = append(keys, key)
			seen[key] = true
		}
		return nil
	})

	// Also include entries table keys not backed by files
	entries, _ := s.List(prefix)
	for _, e := range entries {
		if !seen[e.Key] {
			keys = append(keys, e.Key)
		}
	}

	sort.Strings(keys)
	return keys, nil
}

// ── Push (file/URL → namespace) ────────────────────────────────────────────

// Push adds a file or URL to a namespace in the knowledge base.
// Accepts either a dot-key namespace or a slash path.
func Push(ctx context.Context, s *store.Store, embedder *vectors.Embedder, cfg *config.Config, source, namespace string) error {
	if err := validateNamespacePath(cfg, namespace); err != nil {
		return err
	}

	var content string
	var label string
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		label, content = fetchURL(source)
		if content == "" {
			return fmt.Errorf("failed to fetch %s", source)
		}
	} else {
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read %s: %w", source, err)
		}
		content = string(data)
		label = filepath.Base(source)
	}

	// Write to namespace dir
	nsDir := filepath.Join(cfg.ContextDir(), namespace)
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	slug := slugFromLabel(label)
	destPath := filepath.Join(nsDir, slug+".md")
	if err := os.WriteFile(destPath, []byte(content), 0o644); err != nil {
		return err
	}

	// Build the dot-key for this file
	relPath, _ := filepath.Rel(cfg.ContextDir(), destPath)
	dotKey := PathToKey(relPath)

	// Index in entries table for FTS
	s.Set(dotKey, content, "text/markdown")

	// Register in knowledge table
	sourceID := fmt.Sprintf("ns-%s-%s", strings.ReplaceAll(namespace, "/", "-"), slug)

	s.UpsertKnowledge(&store.KnowledgeItem{
		ID:         sourceID,
		Namespace:  namespace,
		Slug:       slug,
		FilePath:   destPath,
		Summary:    truncate(content, 200),
		Tags:       "[]",
		Source:     source,
		Frequency:  1,
		PromotedAt: time.Now().UTC().Format(time.RFC3339),
	})

	// Embed if embedder available
	if embedder != nil {
		chunks := ChunkMarkdown(content, 400, 50)
		s.DeleteChunksBySource(sourceID)
		for i, chunk := range chunks {
			chunkID := fmt.Sprintf("%s-chunk-%d", sourceID, i)
			vec, err := embedder.Embed(ctx, chunk.Content)
			if err != nil {
				continue
			}
			s.UpsertChunk(&store.Chunk{
				ID:         chunkID,
				SourceID:   sourceID,
				SourcePath: relPath,
				Namespace:  namespace,
				ChunkIndex: i,
				Heading:    chunk.Heading,
				Content:    chunk.Content,
				Vector:     vectors.Pack(vec),
			})
		}
	}

	return nil
}

// ── Note ───────────────────────────────────────────────────────────────────

// Note creates a quick note in the learnings namespace.
func Note(s *store.Store, cfg *config.Config, text string) (string, error) {
	learnDir := filepath.Join(cfg.ContextDir(), "learnings")
	if err := os.MkdirAll(learnDir, 0o755); err != nil {
		return "", err
	}

	now := time.Now().UTC()
	hash := md5.Sum([]byte(now.Format(time.RFC3339Nano)))
	noteID := fmt.Sprintf("note-%x", hash[:4])
	slug := fmt.Sprintf("note-%s-%x", now.Format("2006-01-02"), hash[:4])
	filename := slug + ".md"
	outPath := filepath.Join(learnDir, filename)

	content := fmt.Sprintf(`---
id: %s
type: note
ts: %s
tags: [note]
---

%s
`, noteID, now.Format(time.RFC3339), text)

	if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
		return "", err
	}

	// Also index in entries table for FTS
	dotKey := "learnings." + slug
	s.Set(dotKey, content, "text/markdown")

	return outPath, nil
}

// ── Tag ────────────────────────────────────────────────────────────────────

// Tag adds a tag to a knowledge item or namespace file.
func Tag(s *store.Store, cfg *config.Config, filePath, tag string) error {
	fullPath := resolveContextPath(cfg, filePath)
	if _, err := os.Stat(fullPath); err != nil {
		return fmt.Errorf("file not found: %s", filePath)
	}

	// Try to update knowledge table entry
	items, err := s.ListKnowledge()
	if err != nil {
		return err
	}
	for _, k := range items {
		if k.FilePath == fullPath || strings.HasSuffix(k.FilePath, filePath) {
			var tags []string
			json.Unmarshal([]byte(k.Tags), &tags)
			if !contains(tags, tag) {
				tags = append(tags, tag)
				tagsJSON, _ := json.Marshal(tags)
				k.Tags = string(tagsJSON)
				return s.UpsertKnowledge(&k)
			}
			return nil // already tagged
		}
	}

	return fmt.Errorf("no knowledge entry found for %s", filePath)
}

// ── Forget ─────────────────────────────────────────────────────────────────

// Forget removes a knowledge item by ID.
func Forget(s *store.Store, id string) error {
	items, err := s.ListKnowledge()
	if err != nil {
		return err
	}
	for _, k := range items {
		if k.ID == id || k.Slug == id {
			s.DeleteChunksBySource(k.ID)
			return s.DeleteKnowledge(k.ID)
		}
	}
	return fmt.Errorf("knowledge item %q not found", id)
}

// ── Remove ─────────────────────────────────────────────────────────────────

// Remove deletes a file from the context dir and its associated DB entries.
func Remove(s *store.Store, cfg *config.Config, path string) error {
	fullPath := resolveContextPath(cfg, path)

	if err := validateNamespacePath(cfg, path); err != nil {
		return err
	}

	// Remove file
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	// Clean up entries table (derive dot-key from path)
	relPath, _ := filepath.Rel(cfg.ContextDir(), fullPath)
	if relPath != "" {
		dotKey := PathToKey(relPath)
		s.Delete(dotKey)
	}

	// Clean up DB entries
	items, err := s.ListKnowledge()
	if err != nil {
		return nil
	}
	for _, k := range items {
		if k.FilePath == fullPath || strings.HasSuffix(k.FilePath, path) {
			s.DeleteChunksBySource(k.ID)
			s.DeleteKnowledge(k.ID)
		}
	}

	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func validateNamespacePath(cfg *config.Config, path string) error {
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed: %s", path)
	}
	resolved := resolveContextPath(cfg, path)
	contextDir := cfg.ContextDir()
	if !strings.HasPrefix(resolved, contextDir) {
		return fmt.Errorf("path %s escapes context directory", path)
	}
	return nil
}

func resolveContextPath(cfg *config.Config, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cfg.ContextDir(), path)
}

func slugFromLabel(label string) string {
	s := strings.TrimSuffix(filepath.Base(label), filepath.Ext(label))
	s = strings.ToLower(s)
	s = nonAlpha.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, "-")
	s = dashes.ReplaceAllString(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return strings.TrimRight(s, "-")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
