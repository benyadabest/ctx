package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// CompileResult holds the output of a compile run.
type CompileResult struct {
	MDPath     string
	JSONPath   string
	Approach   string
	Questions  []string
	TraceHits  int
	ChunkHits  int
}

// RankedItem is a search result with a composite score.
type RankedItem struct {
	Type        string  // "learning", "pattern", "skill", "doc", etc.
	ID          string
	FilePath    string
	Heading     string
	Description string
	Score       float64
	Sim         float64
	ProjectTag  string
}

// Compile produces a grounded briefing for a task by searching embeddings + chunks,
// then synthesizing with LLM.
func Compile(ctx context.Context, s *store.Store, client *llm.Client, embedder *vectors.Embedder, cfg *config.Config, task, tag string) (*CompileResult, error) {
	effectiveTask := task
	if effectiveTask == "" {
		effectiveTask = "all knowledge for " + tag
	}

	// Embed the query
	queryVec, err := embedder.Embed(ctx, effectiveTask)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// Search traces (embeddings table)
	traceItems, err := searchTraces(s, queryVec, cfg, tag)
	if err != nil {
		return nil, fmt.Errorf("search traces: %w", err)
	}

	// Search chunks table
	chunkItems, err := searchChunks(s, queryVec, cfg)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}

	// Merge and sort
	allItems := append(traceItems, chunkItems...)
	sortByScore(allItems)

	// Generate briefing via LLM
	briefing, err := generateBriefing(ctx, client, cfg, effectiveTask, allItems)
	if err != nil {
		return nil, fmt.Errorf("generate briefing: %w", err)
	}

	// Build output
	md, structured := buildCompileOutput(effectiveTask, tag, allItems, briefing, cfg)

	// Write files
	compileDir := cfg.Compile.OutputDir
	os.MkdirAll(compileDir, 0o755)
	slug := slugify(effectiveTask)

	mdPath := filepath.Join(compileDir, slug+".md")
	os.WriteFile(mdPath, []byte(md), 0o644)

	jsonPath := filepath.Join(compileDir, slug+".json")
	jsonBytes, _ := json.MarshalIndent(structured, "", "  ")
	os.WriteFile(jsonPath, jsonBytes, 0o644)

	approach, _ := briefing["approach"].(string)
	var questions []string
	if qs, ok := briefing["open_questions"].([]any); ok {
		for _, q := range qs {
			if s, ok := q.(string); ok {
				questions = append(questions, s)
			}
		}
	}

	return &CompileResult{
		MDPath:    mdPath,
		JSONPath:  jsonPath,
		Approach:  approach,
		Questions: questions,
		TraceHits: len(traceItems),
		ChunkHits: len(chunkItems),
	}, nil
}

func searchTraces(s *store.Store, queryVec []float32, cfg *config.Config, tag string) ([]RankedItem, error) {
	summaries, err := s.AllSummaries()
	if err != nil {
		return nil, err
	}
	embeddings, err := s.AllEmbeddings()
	if err != nil {
		return nil, err
	}

	// Build vector lookup
	vecMap := make(map[string][]float32)
	for _, e := range embeddings {
		vecMap[e.TraceID] = vectors.Unpack(e.Vector)
	}

	halflife := float64(cfg.Ranking.RecencyHalflifeDays)
	var items []RankedItem

	for _, sm := range summaries {
		if tag != "" && sm.ProjectTag != tag {
			continue
		}
		vec, ok := vecMap[sm.TraceID]
		if !ok {
			continue
		}

		sim := vectors.CosineSim(queryVec, vec)
		rec := recencyScore(sm.SummarizedAt, halflife)
		score := 0.60*float64(sim) + 0.25*1.0 + 0.15*rec

		desc := sm.EmbeddingText
		if desc == "" {
			desc = sm.Problem
		}

		items = append(items, RankedItem{
			Type:        "learning",
			ID:          sm.TraceID,
			FilePath:    findLearningFile(cfg, sm.TraceID),
			Description: desc,
			Score:       score,
			Sim:         float64(sim),
			ProjectTag:  sm.ProjectTag,
		})
	}

	return items, nil
}

func searchChunks(s *store.Store, queryVec []float32, cfg *config.Config) ([]RankedItem, error) {
	chunks, err := s.AllChunks()
	if err != nil {
		return nil, err
	}

	// Best chunk per source
	bySource := make(map[string]RankedItem)
	for _, c := range chunks {
		if c.Vector == nil {
			continue
		}
		vec := vectors.Unpack(c.Vector)
		sim := vectors.CosineSim(queryVec, vec)

		existing, exists := bySource[c.SourceID]
		if !exists || float64(sim) > existing.Sim {
			nsTypeMap := map[string]string{
				"patterns":        "pattern",
				"debugging":       "debugging",
				"skills/internal": "skill",
				"skills/external": "skill_external",
				"primitives":      "primitive",
				"docs":            "doc",
				"plans":           "plan",
				"learnings":       "learning_chunk",
			}
			itemType := nsTypeMap[c.Namespace]
			if itemType == "" {
				itemType = "doc"
			}

			desc := c.Content
			if len(desc) > 150 {
				desc = desc[:150]
			}

			bySource[c.SourceID] = RankedItem{
				Type:        itemType,
				ID:          c.SourceID,
				FilePath:    c.SourcePath,
				Heading:     c.Heading,
				Description: desc,
				Score:       0.60*float64(sim) + 0.25*0.5 + 0.15*0.5,
				Sim:         float64(sim),
			}
		}
	}

	var items []RankedItem
	for _, item := range bySource {
		items = append(items, item)
	}
	return items, nil
}

func generateBriefing(ctx context.Context, client *llm.Client, cfg *config.Config, task string, ranked []RankedItem) (map[string]any, error) {
	// Build knowledge text for prompt
	var knowledgeLines []string
	limit := min(12, len(ranked))
	for i, it := range ranked[:limit] {
		desc := it.Description
		if len(desc) > 200 {
			desc = desc[:200]
		}
		heading := ""
		if it.Heading != "" && it.Heading != "(full)" {
			heading = fmt.Sprintf(" (section: %s)", it.Heading)
		}
		fp := it.FilePath
		if fp == "" {
			fp = it.ID
		}
		knowledgeLines = append(knowledgeLines, fmt.Sprintf("%d. [%s] %s%s\n   %s", i+1, it.Type, fp, heading, desc))
	}

	tmplText, err := loadPromptFile(cfg, "compile-user.txt")
	if err != nil {
		return nil, err
	}
	system, err := loadPromptFile(cfg, "compile-system.txt")
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("compile").Parse(tmplText)
	if err != nil {
		return nil, err
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, map[string]string{
		"Task":        task,
		"RankedItems": strings.Join(knowledgeLines, "\n"),
	})
	if err != nil {
		return nil, err
	}

	raw, err := client.Call(ctx, "compile", buf.String(), system)
	if err != nil {
		return map[string]any{
			"approach":       fmt.Sprintf("(Could not generate briefing: %v)", err),
			"open_questions": []any{},
		}, nil
	}

	parsed, err := ParseJSONResponse(raw)
	if err != nil {
		return map[string]any{
			"approach":       fmt.Sprintf("(Could not parse briefing: %v)", err),
			"open_questions": []any{},
		}, nil
	}

	return parsed, nil
}

func buildCompileOutput(task, tag string, ranked []RankedItem, briefing map[string]any, cfg *config.Config) (string, map[string]any) {
	now := time.Now().UTC().Format("2006-01-02 15:04")

	topK := cfg.Compile
	patterns := filterByType(ranked, "pattern", topK.TopKPatterns)
	skills := filterByTypes(ranked, []string{"skill", "skill_external"}, topK.TopKSkills)
	learnings := filterByType(ranked, "learning", topK.TopKLearnings)

	approach, _ := briefing["approach"].(string)

	// Build markdown
	var lines []string
	lines = append(lines, "# Context Bundle")
	lines = append(lines, fmt.Sprintf("**Task**: %s", task))
	if tag != "" {
		lines = append(lines, fmt.Sprintf("**Filter**: project_tag=%s", tag))
	}
	lines = append(lines, fmt.Sprintf("**Compiled**: %s", now))
	lines = append(lines, fmt.Sprintf("**Sources**: %d knowledge items", len(patterns)+len(skills)+len(learnings)))
	lines = append(lines, "", "---", "", "## Approach", "", approach)

	renderSection := func(title string, items []RankedItem) {
		if len(items) == 0 {
			return
		}
		lines = append(lines, "", "## "+title)
		for _, it := range items {
			fp := it.FilePath
			if fp == "" {
				fp = it.ID
			}
			heading := ""
			if it.Heading != "" && it.Heading != "(full)" && it.Heading != "(preamble)" {
				heading = fmt.Sprintf("  ## %s", it.Heading)
			}
			extra := ""
			if it.Type == "learning" && it.ProjectTag != "" {
				extra = fmt.Sprintf("  [%s]", it.ProjectTag)
			}
			lines = append(lines, fmt.Sprintf("-> @%s  [%.2f]%s%s", fp, it.Sim, extra, heading))
			desc := it.Description
			if len(desc) > 200 {
				desc = desc[:200]
			}
			lines = append(lines, fmt.Sprintf(`  "%s"`, desc))
			lines = append(lines, "")
		}
	}

	renderSection("Relevant Patterns", patterns)
	renderSection("Relevant Skills", skills)
	renderSection("Relevant Learnings", learnings)

	// Open questions
	lines = append(lines, "---", "", "## Open Questions")
	if qs, ok := briefing["open_questions"].([]any); ok {
		for _, q := range qs {
			if s, ok := q.(string); ok {
				lines = append(lines, "- "+s)
			}
		}
	}
	lines = append(lines, "")

	md := strings.Join(lines, "\n")

	// Build structured JSON
	structured := map[string]any{
		"query":          task,
		"tag":            tag,
		"approach":       approach,
		"open_questions": briefing["open_questions"],
		"sections": map[string]any{
			"patterns":  itemsToJSON(patterns),
			"skills":    itemsToJSON(skills),
			"learnings": itemsToJSON(learnings),
		},
	}

	return md, structured
}

func filterByType(items []RankedItem, typ string, limit int) []RankedItem {
	var out []RankedItem
	for _, it := range items {
		if it.Type == typ {
			out = append(out, it)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func filterByTypes(items []RankedItem, types []string, limit int) []RankedItem {
	typeSet := make(map[string]bool)
	for _, t := range types {
		typeSet[t] = true
	}
	var out []RankedItem
	for _, it := range items {
		if typeSet[it.Type] {
			out = append(out, it)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func itemsToJSON(items []RankedItem) []map[string]any {
	var out []map[string]any
	for _, it := range items {
		out = append(out, map[string]any{
			"path":        it.FilePath,
			"score":       math.Round(it.Sim*100) / 100,
			"heading":     it.Heading,
			"description": it.Description,
			"type":        it.Type,
			"project_tag": it.ProjectTag,
		})
	}
	return out
}

func sortByScore(items []RankedItem) {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].Score > items[i].Score {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

var nonAlpha = regexp.MustCompile(`[^a-z0-9\s-]`)
var spaces = regexp.MustCompile(`[\s]+`)
var dashes = regexp.MustCompile(`-+`)

func slugify(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = nonAlpha.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, "-")
	s = dashes.ReplaceAllString(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return strings.TrimRight(s, "-")
}

func recencyScore(tsStr string, halflifeDays float64) float64 {
	if tsStr == "" {
		return 0.0
	}
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		// Try alternate format
		ts, err = time.Parse("2006-01-02T15:04:05Z", tsStr)
		if err != nil {
			return 0.0
		}
	}
	daysAgo := time.Since(ts).Hours() / 24
	if daysAgo < 0 {
		daysAgo = 0
	}
	return math.Exp(-0.693 * daysAgo / halflifeDays)
}

// RetrieveAndRank embeds a query vector, searches traces + chunks, and returns ranked results.
// Shared by both one-shot compile and streaming chat.
func RetrieveAndRank(s *store.Store, queryVec []float32, cfg *config.Config, tag string) ([]RankedItem, error) {
	traceItems, err := searchTraces(s, queryVec, cfg, tag)
	if err != nil {
		return nil, fmt.Errorf("search traces: %w", err)
	}
	chunkItems, err := searchChunks(s, queryVec, cfg)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	allItems := append(traceItems, chunkItems...)
	sortByScore(allItems)
	return allItems, nil
}

// BuildKnowledgeBlock formats ranked items as a numbered citation block for the LLM system prompt.
func BuildKnowledgeBlock(ranked []RankedItem, limit int) string {
	if limit > len(ranked) {
		limit = len(ranked)
	}
	var lines []string
	for i, it := range ranked[:limit] {
		desc := it.Description
		if len(desc) > 300 {
			desc = desc[:300]
		}
		fp := it.FilePath
		if fp == "" {
			fp = it.ID
		}
		heading := ""
		if it.Heading != "" && it.Heading != "(full)" {
			heading = fmt.Sprintf(" (section: %s)", it.Heading)
		}
		tag := ""
		if it.ProjectTag != "" {
			tag = fmt.Sprintf(" project=%s", it.ProjectTag)
		}
		lines = append(lines, fmt.Sprintf("[%d] %s=%s%s sim=%.2f%s\n    %s",
			i+1, it.Type, fp, heading, it.Sim, tag, desc))
	}
	if len(lines) == 0 {
		return "(No relevant knowledge found.)"
	}
	return strings.Join(lines, "\n\n")
}

// SourceRef is a citation reference returned to the UI.
type SourceRef struct {
	Index       int     `json:"index"`
	Type        string  `json:"type"`
	Path        string  `json:"path"`
	Heading     string  `json:"heading,omitempty"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
	ProjectTag  string  `json:"project_tag,omitempty"`
}

// RankedToSourceList converts ranked items to a list of source references for the UI.
func RankedToSourceList(ranked []RankedItem, limit int) []SourceRef {
	if limit > len(ranked) {
		limit = len(ranked)
	}
	refs := make([]SourceRef, limit)
	for i, it := range ranked[:limit] {
		fp := it.FilePath
		if fp == "" {
			fp = it.ID
		}
		desc := it.Description
		if len(desc) > 150 {
			desc = desc[:150]
		}
		refs[i] = SourceRef{
			Index:       i + 1,
			Type:        it.Type,
			Path:        fp,
			Heading:     it.Heading,
			Description: desc,
			Score:       math.Round(it.Sim*100) / 100,
			ProjectTag:  it.ProjectTag,
		}
	}
	return refs
}

func findLearningFile(cfg *config.Config, traceID string) string {
	learnDir := filepath.Join(cfg.ContextDir(), "learnings")
	entries, err := os.ReadDir(learnDir)
	if err != nil {
		return "learnings/" + traceID + ".md"
	}
	needle := "id: " + traceID
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(learnDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) > 500 {
			data = data[:500]
		}
		if strings.Contains(string(data), needle) {
			return "learnings/" + entry.Name()
		}
	}
	return "learnings/" + traceID + ".md"
}
