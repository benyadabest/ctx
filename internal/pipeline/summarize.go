package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/store"
)

// SummarizeResult holds stats from a batch summarize run.
type SummarizeResult struct {
	Total     int
	Succeeded int
	Failed    int
}

// SummarizeTrace summarizes a single trace using LLM extraction.
// Returns the summary on success.
func SummarizeTrace(ctx context.Context, s *store.Store, client *llm.Client, cfg *config.Config, trace *store.Trace) (*store.Summary, error) {
	prompt, err := buildSummarizePrompt(cfg, trace)
	if err != nil {
		return nil, fmt.Errorf("build prompt: %w", err)
	}

	system, err := loadPromptFile(cfg, "summarize-system.txt")
	if err != nil {
		return nil, err
	}

	// Attempt LLM call (with one retry on parse failure)
	var parsed map[string]any
	raw, err := client.Call(ctx, "summarize", prompt, system)
	if err != nil {
		return nil, fmt.Errorf("llm call: %w", err)
	}

	parsed, err = ParseJSONResponse(raw)
	if err != nil {
		// Retry once
		raw, err = client.Call(ctx, "summarize", prompt, system)
		if err != nil {
			return nil, fmt.Errorf("llm retry: %w", err)
		}
		parsed, err = ParseJSONResponse(raw)
		if err != nil {
			return nil, fmt.Errorf("json parse after retry: %w (raw: %.200s)", err, raw)
		}
	}

	// Extract portable + domain blocks
	portable, _ := parsed["portable"].(map[string]any)
	if portable == nil {
		portable = parsed // fallback: flat structure
	}
	domain, _ := parsed["domain"].(map[string]any)
	if domain == nil {
		domain = map[string]any{}
	}

	// Validate required keys
	for _, key := range []string{"problem", "pattern", "skills", "pitfalls", "embedding_text"} {
		if _, ok := portable[key]; !ok {
			return nil, fmt.Errorf("missing portable key %q in LLM response", key)
		}
	}

	// Normalize skills to JSON array
	skills := portable["skills"]
	var skillsJSON string
	switch v := skills.(type) {
	case []any:
		b, _ := json.Marshal(v)
		skillsJSON = string(b)
	default:
		b, _ := json.Marshal([]any{v})
		skillsJSON = string(b)
	}

	projectTag := stringVal(domain, "project_tag")
	if projectTag == "" {
		projectTag = trace.Project
		if projectTag == "" {
			projectTag = "unknown"
		}
	}
	component := stringVal(domain, "component")
	domainKnowledge := stringVal(domain, "knowledge")

	now := time.Now().UTC().Format(time.RFC3339)

	sm := &store.Summary{
		TraceID:         trace.ID,
		Problem:         stringVal(portable, "problem"),
		Pattern:         stringVal(portable, "pattern"),
		Skills:          skillsJSON,
		Pitfalls:        stringVal(portable, "pitfalls"),
		EmbeddingText:   stringVal(portable, "embedding_text"),
		ProjectTag:      projectTag,
		Component:       component,
		DomainKnowledge: domainKnowledge,
		SummarizedAt:    now,
	}

	if err := s.UpsertSummary(sm); err != nil {
		return nil, err
	}

	// Update meta timestamp
	s.SetMeta("last_summarize_ts", now)

	// Update learning .md file with summary block
	updateLearningMD(cfg, trace.ID, portable, domain, projectTag)

	return sm, nil
}

// SummarizeAll processes all unsummarized traces (or all if reprocess=true).
func SummarizeAll(ctx context.Context, s *store.Store, client *llm.Client, cfg *config.Config, reprocess bool) (*SummarizeResult, error) {
	var traces []store.Trace
	var err error

	if reprocess {
		traces, err = allTraces(s)
	} else {
		traces, err = s.UnsummarizedTraces()
	}
	if err != nil {
		return nil, err
	}

	result := &SummarizeResult{Total: len(traces)}
	for i := range traces {
		_, err := SummarizeTrace(ctx, s, client, cfg, &traces[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: %v\n", traces[i].ID, err)
			result.Failed++
			continue
		}
		result.Succeeded++
	}
	return result, nil
}

func allTraces(s *store.Store) ([]store.Trace, error) {
	return s.ListTraces()
}

func buildSummarizePrompt(cfg *config.Config, trace *store.Trace) (string, error) {
	// Build session block
	var parts []string
	if trace.Prompt != "" {
		p := trace.Prompt
		if len(p) > 2000 {
			p = p[:2000]
		}
		parts = append(parts, "Prompt: "+p)
	} else {
		parts = append(parts, "Prompt: (none)")
	}

	if trace.FilesModified != "" {
		var files []string
		if err := json.Unmarshal([]byte(trace.FilesModified), &files); err == nil && len(files) > 0 {
			if len(files) > 20 {
				files = files[:20]
			}
			parts = append(parts, "Files modified: "+strings.Join(files, ", "))
		}
	}

	parts = append(parts, "Status: "+trace.Status)
	parts = append(parts, "Workspace: "+trace.Workspace)
	parts = append(parts, fmt.Sprintf("Loop count: %d", trace.LoopCount))

	sessionBlock := strings.Join(parts, "\n")

	tmplText, err := loadPromptFile(cfg, "summarize-user.txt")
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("summarize").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, map[string]string{"SessionBlock": sessionBlock})
	if err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}

	return buf.String(), nil
}

func loadPromptFile(cfg *config.Config, name string) (string, error) {
	// Check embedded prompts directory relative to the binary,
	// then fall back to prompts/ in the repo root
	candidates := []string{
		filepath.Join(cfg.ContextDir(), "prompts", name),
		filepath.Join("prompts", name),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("prompt file %q not found", name)
}

var oldSummaryBlock = regexp.MustCompile(`(?s)\n## AI Summary\n.*`)

func updateLearningMD(cfg *config.Config, traceID string, portable, domain map[string]any, projectTag string) {
	learnDir := filepath.Join(cfg.ContextDir(), "learnings")
	entries, err := os.ReadDir(learnDir)
	if err != nil {
		return
	}

	needle := "id: " + traceID
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(learnDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(content)
		if !strings.Contains(text, needle) {
			continue
		}

		// Add project_tag to frontmatter if missing
		if !strings.Contains(text, "project_tag:") {
			text = strings.Replace(text, needle, needle+"\nproject_tag: "+projectTag, 1)
		}

		// Remove old summary block
		text = oldSummaryBlock.ReplaceAllString(text, "")

		// Build new summary block
		skills := ""
		if sl, ok := portable["skills"].([]any); ok {
			strs := make([]string, len(sl))
			for i, v := range sl {
				strs[i] = fmt.Sprintf("%v", v)
			}
			skills = strings.Join(strs, ", ")
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "\n## AI Summary\n\n")
		fmt.Fprintf(&sb, "**Problem**: %s\n", stringVal(portable, "problem"))
		fmt.Fprintf(&sb, "**Pattern**: %s\n", stringVal(portable, "pattern"))
		fmt.Fprintf(&sb, "**Skills**: %s\n", skills)
		fmt.Fprintf(&sb, "**Pitfalls**: %s\n\n", stringVal(portable, "pitfalls"))
		fmt.Fprintf(&sb, "> %s\n", stringVal(portable, "embedding_text"))

		comp := stringVal(domain, "component")
		know := stringVal(domain, "knowledge")
		if comp != "" || know != "" {
			fmt.Fprintf(&sb, "\n## Domain\n\n")
			fmt.Fprintf(&sb, "**Project**: %s\n", projectTag)
			fmt.Fprintf(&sb, "**Component**: %s\n", comp)
			fmt.Fprintf(&sb, "**Knowledge**: %s\n", know)
		}

		os.WriteFile(path, []byte(strings.TrimRight(text, "\n")+"\n"+sb.String()), 0o644)
		return
	}
}

func stringVal(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}
