package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/store"
)

// DetectResult holds stats from a detection run.
type DetectResult struct {
	Candidates int
	Skipped    int
}

// DetectCandidate represents something to synthesize.
type DetectCandidate struct {
	Kind   string // "slug", "pitfall", "pair"
	Slug   string
	Traces []traceSummary
}

type traceSummary struct {
	TraceID       string
	Problem       string
	Pattern       string
	Skills        string
	Pitfalls      string
	EmbeddingText string
}

// Detect runs pattern detection across all summaries, finding recurring skills,
// repeated pitfalls, and co-occurring slug pairs.
func Detect(ctx context.Context, s *store.Store, client *llm.Client, cfg *config.Config, force, dryRun bool) (*DetectResult, error) {
	// Check trace counter gate unless forced
	if !force && !dryRun {
		batchSize := cfg.Detection.BatchSize
		tracesSince := countTracesSinceDetection(s)
		if tracesSince < batchSize {
			return nil, fmt.Errorf("%d/%d traces since last detection (use --force to run anyway)", tracesSince, batchSize)
		}
	}

	minFreq := cfg.Detection.MinFreq

	// Get existing candidates (pending/approved)
	existing, err := s.ExistingCandidateSlugs()
	if err != nil {
		return nil, err
	}

	// Build slug and pitfall frequency maps
	slugFreq, pitfallFreq, err := buildFrequencyMaps(s)
	if err != nil {
		return nil, err
	}

	// Collect candidates to synthesize
	var toSynthesize []DetectCandidate

	// 1. Skill slugs appearing min_freq+ times
	for slug, traces := range slugFreq {
		if len(traces) < minFreq {
			continue
		}
		if existing[slug] {
			continue
		}
		toSynthesize = append(toSynthesize, DetectCandidate{Kind: "slug", Slug: slug, Traces: traces})
	}

	// 2. Repeated pitfalls
	for key, traces := range pitfallFreq {
		if len(traces) < minFreq {
			continue
		}
		pitfallSlug := strings.ReplaceAll(key, " ", "-")
		if len(pitfallSlug) > 30 {
			pitfallSlug = pitfallSlug[:30]
		}
		if existing[pitfallSlug] {
			continue
		}
		toSynthesize = append(toSynthesize, DetectCandidate{Kind: "pitfall", Slug: pitfallSlug, Traces: traces})
	}

	// 3. Co-occurring slug pairs
	for slug, traces := range slugFreq {
		traceIDs := make(map[string]bool)
		for _, t := range traces {
			traceIDs[t.TraceID] = true
		}
		for otherSlug, otherTraces := range slugFreq {
			if otherSlug <= slug {
				continue
			}
			var overlap []traceSummary
			for _, t := range otherTraces {
				if traceIDs[t.TraceID] {
					overlap = append(overlap, t)
				}
			}
			if len(overlap) >= minFreq {
				pairKey := slug + "+" + otherSlug
				if !existing[pairKey] {
					toSynthesize = append(toSynthesize, DetectCandidate{Kind: "pair", Slug: pairKey, Traces: overlap})
				}
			}
		}
	}

	if len(toSynthesize) == 0 {
		return &DetectResult{}, nil
	}

	// Dry run
	if dryRun {
		for _, c := range toSynthesize {
			ids := make([]string, 0, min(5, len(c.Traces)))
			for _, t := range c.Traces[:min(5, len(c.Traces))] {
				ids = append(ids, t.TraceID)
			}
			fmt.Printf("  [%s] %s — %d traces: %s\n", c.Kind, c.Slug, len(c.Traces), strings.Join(ids, ", "))
		}
		return &DetectResult{Candidates: len(toSynthesize)}, nil
	}

	// Synthesize each candidate
	result := &DetectResult{}
	for _, cand := range toSynthesize {
		err := synthesizeCandidate(ctx, s, client, cfg, cand)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: %v\n", cand.Slug, err)
			result.Skipped++
			continue
		}
		result.Candidates++
	}

	// Update meta
	now := time.Now().UTC().Format(time.RFC3339)
	s.SetMeta("last_detection_run", now)
	s.SetMeta("traces_since_detection", "0")

	return result, nil
}

func synthesizeCandidate(ctx context.Context, s *store.Store, client *llm.Client, cfg *config.Config, cand DetectCandidate) error {
	prompt, err := buildDetectPrompt(cfg, cand)
	if err != nil {
		return err
	}

	system, err := loadPromptFile(cfg, "detect-system.txt")
	if err != nil {
		return err
	}

	raw, err := client.Call(ctx, "detect", prompt, system)
	if err != nil {
		return fmt.Errorf("llm call: %w", err)
	}

	parsed, err := ParseJSONResponse(raw)
	if err != nil {
		// Retry once
		raw, err = client.Call(ctx, "detect", prompt, system)
		if err != nil {
			return fmt.Errorf("llm retry: %w", err)
		}
		parsed, err = ParseJSONResponse(raw)
		if err != nil {
			return fmt.Errorf("json parse after retry: %w", err)
		}
	}

	// Null escape hatch
	if parsed["namespace"] == nil {
		return fmt.Errorf("model returned null (no generalizable pattern)")
	}

	// Validate required keys
	for _, key := range []string{"namespace", "slug", "title", "body"} {
		if _, ok := parsed[key]; !ok {
			return fmt.Errorf("missing key %q", key)
		}
	}

	ns := stringVal(parsed, "namespace")
	validNS := map[string]bool{"skills/internal": true, "patterns": true, "debugging": true, "primitives": true}
	if !validNS[ns] {
		return fmt.Errorf("invalid namespace: %s", ns)
	}

	return writeCandidate(s, cfg, parsed, cand)
}

func writeCandidate(s *store.Store, cfg *config.Config, result map[string]any, cand DetectCandidate) error {
	ns := stringVal(result, "namespace")
	slug := stringVal(result, "slug")
	title := stringVal(result, "title")
	body := stringVal(result, "body")

	typeMap := map[string]string{
		"skills/internal": "skill",
		"patterns":        "pattern",
		"debugging":       "debugging",
		"primitives":      "primitive",
	}
	candType := typeMap[ns]
	if candType == "" {
		candType = "pattern"
	}

	candID := fmt.Sprintf("candidate-%s-%s", strings.ReplaceAll(ns, "/", "-"), slug)
	now := time.Now().UTC().Format(time.RFC3339)

	traceIDs := make([]string, len(cand.Traces))
	for i, t := range cand.Traces {
		traceIDs[i] = t.TraceID
	}
	traceIDsJSON, _ := json.Marshal(traceIDs)

	// Write markdown file
	candDir := filepath.Join(cfg.ContextDir(), "candidates")
	os.MkdirAll(candDir, 0o755)
	mdPath := filepath.Join(candDir, fmt.Sprintf("candidate-%s-%s.md", candType, slug))

	signal := 3
	if v, ok := result["signal_strength"].(float64); ok {
		signal = int(v)
	}

	md := fmt.Sprintf(`---
id: %s
type: %s
status: pending
detected_at: %s
frequency: %d
signal_strength: %d
source_traces: [%s]
target_namespace: %s
---

# %s

%s
`, candID, candType, now, len(cand.Traces), signal,
		strings.Join(traceIDs, ", "), ns, title, body)

	os.WriteFile(mdPath, []byte(md), 0o644)

	// Upsert to DB
	return s.UpsertCandidate(&store.Candidate{
		ID:           candID,
		Type:         candType,
		Slug:         slug,
		Definition:   body,
		Status:       "pending",
		Frequency:    len(cand.Traces),
		SourceTraces: string(traceIDsJSON),
		DetectedAt:   now,
	})
}

func buildDetectPrompt(cfg *config.Config, cand DetectCandidate) (string, error) {
	tmplText, err := loadPromptFile(cfg, "detect-user.txt")
	if err != nil {
		return "", err
	}

	// Build trace summaries text
	var parts []string
	for i, ts := range cand.Traces {
		parts = append(parts, fmt.Sprintf("\n--- Trace %d (%s) ---", i+1, ts.TraceID))
		parts = append(parts, "Problem: "+ts.Problem)
		parts = append(parts, "Pattern: "+ts.Pattern)
		parts = append(parts, "Skills: "+ts.Skills)
		parts = append(parts, "Pitfalls: "+ts.Pitfalls)
		parts = append(parts, "Summary: "+ts.EmbeddingText)
	}

	tmpl, err := template.New("detect").Parse(tmplText)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, map[string]any{
		"TraceCount":     len(cand.Traces),
		"Slug":           cand.Slug,
		"TraceSummaries": strings.Join(parts, "\n"),
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func buildFrequencyMaps(s *store.Store) (slugMap map[string][]traceSummary, pitfallMap map[string][]traceSummary, err error) {
	summaries, err := s.AllSummaries()
	if err != nil {
		return nil, nil, err
	}

	slugMap = make(map[string][]traceSummary)
	pitfallMap = make(map[string][]traceSummary)

	for _, sm := range summaries {
		ts := traceSummary{
			TraceID:       sm.TraceID,
			Problem:       sm.Problem,
			Pattern:       sm.Pattern,
			Skills:        sm.Skills,
			Pitfalls:      sm.Pitfalls,
			EmbeddingText: sm.EmbeddingText,
		}

		// Skills
		var skills []string
		if err := json.Unmarshal([]byte(sm.Skills), &skills); err == nil {
			for _, slug := range skills {
				slug = strings.TrimSpace(strings.ToLower(slug))
				if slug != "" {
					slugMap[slug] = append(slugMap[slug], ts)
				}
			}
		}

		// Pitfalls (group by first 50 chars)
		if sm.Pitfalls != "" {
			key := strings.ToLower(strings.TrimSpace(sm.Pitfalls))
			if len(key) > 50 {
				key = key[:50]
			}
			pitfallMap[key] = append(pitfallMap[key], ts)
		}
	}

	return slugMap, pitfallMap, nil
}

func countTracesSinceDetection(s *store.Store) int {
	lastRun, _ := s.GetMeta("last_detection_run")
	if lastRun == "" {
		// Count all summaries
		sums, _ := s.AllSummaries()
		return len(sums)
	}
	// Count summaries after last detection run
	sums, _ := s.AllSummaries()
	count := 0
	for _, sm := range sums {
		if sm.SummarizedAt > lastRun {
			count++
		}
	}
	return count
}
