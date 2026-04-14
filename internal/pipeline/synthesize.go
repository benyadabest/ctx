package pipeline

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
)

// SynthesizeOpts configures a synthesis run.
type SynthesizeOpts struct {
	Sources   []string // URLs or file paths
	Output    string   // output path relative to context dir, e.g. "skills/external/eval-design"
	Type      string   // "skill" or "pattern"
	Context   string   // calibration context
	Refine    string   // path to existing skill to refine
	Feedback  string   // what to improve (used with Refine)
}

// SynthesizeResult holds the output of a synthesis.
type SynthesizeResult struct {
	FilePath string
	SourceID string
	Chunks   int
}

// Synthesize creates or refines a skill/pattern from source material.
func Synthesize(ctx context.Context, s *store.Store, client *llm.Client, embedder *vectors.Embedder, cfg *config.Config, opts SynthesizeOpts) (*SynthesizeResult, error) {
	if opts.Type == "" {
		opts.Type = "skill"
	}

	// Handle refine mode
	var existingContent string
	version := 1
	if opts.Refine != "" {
		content, ver, err := loadExistingSkill(cfg, opts.Refine)
		if err != nil {
			return nil, err
		}
		existingContent = content
		version = ver + 1
		if opts.Output == "" {
			opts.Output = opts.Refine
		}
	}

	if opts.Output == "" && opts.Refine == "" {
		return nil, fmt.Errorf("--output required (or use --refine)")
	}

	// Fetch sources
	var fetched []fetchedSource
	var sourceLabels []string
	for _, src := range opts.Sources {
		label, content := fetchSource(cfg, src)
		if content != "" {
			fetched = append(fetched, fetchedSource{label: label, content: content})
			sourceLabels = append(sourceLabels, label)
		}
	}

	if len(fetched) == 0 && existingContent == "" {
		return nil, fmt.Errorf("no source content to synthesize")
	}

	// Combine sources
	var combined strings.Builder
	for _, f := range fetched {
		content := f.content
		if len(content) > 4000 {
			content = content[:4000]
		}
		fmt.Fprintf(&combined, "\n--- Source: %s ---\n%s\n", f.label, content)
	}

	// Build prompt
	prompt, err := buildSynthPrompt(cfg, combined.String(), opts.Context, opts.Type, existingContent, opts.Feedback)
	if err != nil {
		return nil, err
	}

	system, err := loadPromptFile(cfg, "synthesize-system.txt")
	if err != nil {
		return nil, err
	}

	// Call LLM
	raw, err := client.Call(ctx, "synthesize", prompt, system)
	if err != nil {
		return nil, fmt.Errorf("llm call: %w", err)
	}

	parsed, err := ParseJSONResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("json parse: %w (raw: %.300s)", err, raw)
	}

	// Write output
	outputDir := filepath.Join(cfg.ContextDir(), opts.Output)
	skillPath, err := writeSkillMD(outputDir, parsed, opts.Type, version, sourceLabels)
	if err != nil {
		return nil, err
	}

	// Store URL references
	storeReferences(outputDir, fetched)

	// Determine namespace for knowledge table
	ns := deriveNamespace(opts.Output)
	slug := filepath.Base(outputDir)
	sourceID := fmt.Sprintf("synth-%s-%s", strings.ReplaceAll(ns, "/", "-"), slug)

	// Chunk + embed
	relPath, _ := filepath.Rel(cfg.ContextDir(), skillPath)
	chunks := chunkAndEmbed(ctx, s, embedder, skillPath, sourceID, relPath, ns)

	// Add to knowledge table
	description, _ := parsed["description"].(string)
	var tags []string
	if tagList, ok := parsed["tags"].([]any); ok {
		for _, t := range tagList {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	tagsJSON, _ := json.Marshal(tags)

	now := time.Now().UTC().Format(time.RFC3339)
	s.UpsertKnowledge(&store.KnowledgeItem{
		ID:         sourceID,
		Namespace:  ns,
		Slug:       slug,
		FilePath:   skillPath,
		Summary:    description,
		Tags:       string(tagsJSON),
		Source:     "synthesized",
		Frequency:  1,
		PromotedAt: now,
	})

	// Log to synthesis_history
	hash := md5.Sum([]byte(now))
	histID := fmt.Sprintf("synth-%x", hash[:4])
	sourcesJSON, _ := json.Marshal(sourceLabels)

	s.LogSynthesis(histID, relPath, version, string(sourcesJSON), opts.Feedback, cfg.Models.Synthesize)

	return &SynthesizeResult{
		FilePath: skillPath,
		SourceID: sourceID,
		Chunks:   chunks,
	}, nil
}

// ── Internal helpers ────────────────────────────────────────────────────────

type fetchedSource struct {
	label   string
	content string
}

func fetchSource(cfg *config.Config, source string) (string, string) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return fetchURL(source)
	}
	// Try relative to context dir first, then absolute
	p := filepath.Join(cfg.ContextDir(), source)
	if _, err := os.Stat(p); err != nil {
		p = source
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return source, ""
	}
	return source, string(data)
}

func fetchURL(url string) (string, string) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return url, ""
	}
	req.Header.Set("User-Agent", "ctx/1.0")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return url, ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return url, ""
	}

	text := string(body)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "html") {
		text = stripHTML(text)
	}
	if len(text) > 8000 {
		text = text[:8000]
	}
	return url, text
}

var scriptTag = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
var styleTag = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
var htmlTag = regexp.MustCompile(`<[^>]+>`)
var multiNewline = regexp.MustCompile(`\n{3,}`)

func stripHTML(s string) string {
	s = scriptTag.ReplaceAllString(s, "")
	s = styleTag.ReplaceAllString(s, "")
	s = htmlTag.ReplaceAllString(s, "")
	s = multiNewline.ReplaceAllString(s, "\n\n")
	return s
}

func loadExistingSkill(cfg *config.Config, path string) (string, int, error) {
	fullPath := filepath.Join(cfg.ContextDir(), path)
	if _, err := os.Stat(fullPath); err != nil {
		// Try adding skill.md
		fullPath = filepath.Join(cfg.ContextDir(), path, "skill.md")
		if _, err := os.Stat(fullPath); err != nil {
			return "", 0, fmt.Errorf("cannot find %s", path)
		}
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", 0, err
	}
	content := string(data)

	// Extract version from frontmatter
	version := 1
	versionRe := regexp.MustCompile(`(?m)^version:\s*(\d+)`)
	if m := versionRe.FindStringSubmatch(content); len(m) > 1 {
		fmt.Sscanf(m[1], "%d", &version)
	}

	return content, version, nil
}

func buildSynthPrompt(cfg *config.Config, sourcesCombined, ctxStr, synthType, existingContent, feedback string) (string, error) {
	tmplText, err := loadPromptFile(cfg, "synthesize-user.txt")
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("synthesize").Parse(tmplText)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, map[string]string{
		"Sources":       sourcesCombined,
		"Context":       ctxStr,
		"ExistingSkill": existingContent,
		"Feedback":      feedback,
		"Type":          synthType,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func writeSkillMD(outputDir string, result map[string]any, synthType string, version int, sourceLabels []string) (string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", err
	}

	name := stringVal(result, "name")
	if name == "" {
		name = filepath.Base(outputDir)
	}
	now := time.Now().UTC().Format("2006-01-02")

	var tags []string
	if tagList, ok := result["tags"].([]any); ok {
		for _, t := range tagList {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}

	var process []string
	if procList, ok := result["process"].([]any); ok {
		for i, p := range procList {
			if s, ok := p.(string); ok {
				process = append(process, fmt.Sprintf("%d. %s", i+1, s))
			}
		}
	}

	var principles []string
	if prinList, ok := result["key_principles"].([]any); ok {
		for _, p := range prinList {
			if s, ok := p.(string); ok {
				principles = append(principles, "- "+s)
			}
		}
	}

	var pitfalls []string
	if pitList, ok := result["pitfalls"].([]any); ok {
		for _, p := range pitList {
			if s, ok := p.(string); ok {
				pitfalls = append(pitfalls, "- "+s)
			}
		}
	}

	var sources []string
	if srcList, ok := result["sources_used"].([]any); ok {
		for _, s := range srcList {
			if str, ok := s.(string); ok {
				sources = append(sources, "- "+str)
			}
		}
	}
	if len(sources) == 0 {
		for _, s := range sourceLabels {
			sources = append(sources, "- "+s)
		}
	}

	filename := "skill.md"
	if synthType == "pattern" {
		filename = "pattern.md"
	}
	outPath := filepath.Join(outputDir, filename)

	title := strings.ReplaceAll(name, "-", " ")
	title = strings.Title(title) //nolint:staticcheck

	content := fmt.Sprintf(`---
id: %s-%s
type: %s
version: %d
synthesized_at: %s
sources: [%s]
tags: [%s]
---

# %s

%s

## When to Use

%s

## Process

%s

## Key Principles

%s

## Pitfalls

%s

## Sources

%s
`,
		synthType, name,
		synthType,
		version,
		now,
		strings.Join(sourceLabels, ", "),
		strings.Join(tags, ", "),
		title,
		stringVal(result, "description"),
		stringVal(result, "when_to_use"),
		strings.Join(process, "\n"),
		strings.Join(principles, "\n"),
		strings.Join(pitfalls, "\n"),
		strings.Join(sources, "\n"),
	)

	if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func chunkAndEmbed(ctx context.Context, s *store.Store, embedder *vectors.Embedder, filePath, sourceID, relPath, namespace string) int {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0
	}

	chunks := ChunkMarkdown(string(data), 400, 50)
	s.DeleteChunksBySource(sourceID)

	created := 0
	for i, chunk := range chunks {
		chunkID := fmt.Sprintf("%s-chunk-%d", sourceID, i)
		vec, err := embedder.Embed(ctx, chunk.Content)
		if err != nil {
			continue
		}
		err = s.UpsertChunk(&store.Chunk{
			ID:         chunkID,
			SourceID:   sourceID,
			SourcePath: relPath,
			Namespace:  namespace,
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
	return created
}

func storeReferences(outputDir string, fetched []fetchedSource) {
	for _, f := range fetched {
		if !strings.HasPrefix(f.label, "http") || f.content == "" {
			continue
		}
		refsDir := filepath.Join(outputDir, "references")
		os.MkdirAll(refsDir, 0o755)
		slug := strings.TrimPrefix(f.label, "https://")
		slug = strings.TrimPrefix(slug, "http://")
		slug = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(slug), "-")
		if len(slug) > 40 {
			slug = slug[:40]
		}
		slug = strings.TrimRight(slug, "-")
		content := f.content
		if len(content) > 5000 {
			content = content[:5000]
		}
		os.WriteFile(filepath.Join(refsDir, slug+".md"),
			[]byte(fmt.Sprintf("# Source: %s\n\n%s\n", f.label, content)), 0o644)
	}
}

func deriveNamespace(output string) string {
	if strings.Contains(output, "skills/internal") {
		return "skills/internal"
	}
	if strings.Contains(output, "skills/external") {
		return "skills/external"
	}
	if strings.Contains(output, "patterns") {
		return "patterns"
	}
	return "skills/external"
}
