package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/pipeline"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
)

//go:embed index.html
var indexHTML string

// Serve starts the web UI on 127.0.0.1:<port>.
func Serve(s *store.Store, cfg *config.Config) error {
	h := &handler{store: s, cfg: cfg}

	mux := http.NewServeMux()

	// HTML
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})

	// API endpoints
	mux.HandleFunc("/api/stats", h.apiStats)
	mux.HandleFunc("/api/tags", h.apiTags)
	mux.HandleFunc("/api/traces", h.apiTraces)
	mux.HandleFunc("/api/traces/by-skill/", h.apiTracesBySkill)
	mux.HandleFunc("/api/compile", h.apiCompile)
	mux.HandleFunc("/api/chat", h.apiChat)
	mux.HandleFunc("/api/synthesize", h.apiSynthesize)
	mux.HandleFunc("/api/candidates", h.apiCandidates)
	mux.HandleFunc("/api/candidates/", h.apiCandidateAction)
	mux.HandleFunc("/api/namespace", h.apiNamespace)
	mux.HandleFunc("/api/file", h.apiFile)
	mux.HandleFunc("/api/file/upload", h.apiFileUpload)
	mux.HandleFunc("/api/file/move", h.apiFileMove)
	mux.HandleFunc("/api/file/tags", h.apiFileTags)
	mux.HandleFunc("/api/tags/edit", h.apiTagsEdit)
	mux.HandleFunc("/api/graph", h.apiGraph)
	mux.HandleFunc("/api/knowledge", h.apiKnowledge)
	mux.HandleFunc("/api/search", h.apiSearch)
	mux.HandleFunc("/api/park", h.apiPark)
	mux.HandleFunc("/api/skills", h.apiSkills)
	mux.HandleFunc("/api/skills/deploy", h.apiSkillDeploy)
	mux.HandleFunc("/api/skills/undeploy", h.apiSkillUndeploy)
	mux.HandleFunc("/api/usage", h.apiUsage)
	mux.HandleFunc("/api/delete", h.apiDelete)

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Serve.Port)
	fmt.Printf("ctx serve: http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

type handler struct {
	store *store.Store
	cfg   *config.Config
}

// ── Stats ───────────────────────────────────────────────────────────────────

func (h *handler) apiStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.Stats()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	candidates, _ := h.store.ListCandidates("")
	pending, approved, dismissed := 0, 0, 0
	for _, c := range candidates {
		switch c.Status {
		case "pending":
			pending++
		case "approved":
			approved++
		case "dismissed":
			dismissed++
		}
	}
	result := map[string]int{
		"traces":     stats["traces"],
		"summaries":  stats["summaries"],
		"embeddings": stats["embeddings"],
		"chunks":     stats["chunks"],
		"knowledge":  stats["knowledge"],
		"pending":    pending,
		"approved":   approved,
		"dismissed":  dismissed,
	}
	jsonResp(w, result)
}

// ── Tags ────────────────────────────────────────────────────────────────────

func (h *handler) apiTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.store.AllProjectTags()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, tags)
}

// ── Traces ──────────────────────────────────────────────────────────────────

func (h *handler) apiTraces(w http.ResponseWriter, r *http.Request) {
	traces, err := h.store.ListTraces()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	summaries, _ := h.store.AllSummaries()
	sumMap := make(map[string]*store.Summary)
	for i := range summaries {
		sumMap[summaries[i].TraceID] = &summaries[i]
	}

	var result []map[string]any
	for _, t := range traces {
		item := map[string]any{
			"id": t.ID, "ts": t.TS, "source": t.Source, "status": t.Status,
			"project": t.Project, "prompt": t.Prompt, "files_modified": t.FilesModified,
			"loop_count": t.LoopCount,
		}
		if sm, ok := sumMap[t.ID]; ok {
			item["problem"] = sm.Problem
			item["pattern"] = sm.Pattern
			item["skills"] = sm.Skills
			item["pitfalls"] = sm.Pitfalls
			item["embedding_text"] = sm.EmbeddingText
			item["project_tag"] = sm.ProjectTag
			item["component"] = sm.Component
			item["domain_knowledge"] = sm.DomainKnowledge
		}
		result = append(result, item)
	}
	jsonResp(w, result)
}

func (h *handler) apiTracesBySkill(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/traces/by-skill/")
	if slug == "" {
		jsonError(w, "slug required", 400)
		return
	}

	traces, err := h.store.ListTraces()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	summaries, _ := h.store.AllSummaries()
	sumMap := make(map[string]*store.Summary)
	for i := range summaries {
		sumMap[summaries[i].TraceID] = &summaries[i]
	}

	var result []map[string]any
	pattern := fmt.Sprintf(`"%s"`, slug)
	for _, t := range traces {
		sm, ok := sumMap[t.ID]
		if !ok || !strings.Contains(sm.Skills, pattern) {
			continue
		}
		result = append(result, map[string]any{
			"id": t.ID, "ts": t.TS, "source": t.Source, "project": t.Project,
			"prompt": t.Prompt, "problem": sm.Problem, "pattern": sm.Pattern,
			"skills": sm.Skills, "embedding_text": sm.EmbeddingText,
		})
	}
	jsonResp(w, result)
}

// ── Compile ─────────────────────────────────────────────────────────────────

func (h *handler) apiCompile(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Task string `json:"task"`
		Tag  string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", 400)
		return
	}
	if body.Task == "" && body.Tag == "" {
		jsonError(w, "task or tag required", 400)
		return
	}

	client, err := llm.New(h.cfg, h.store)
	if err != nil {
		jsonError(w, "llm init: "+err.Error(), 500)
		return
	}
	embedder := vectors.New(h.cfg.API.OllamaBaseURL, "all-minilm")
	ctx := context.Background()

	result, err := pipeline.Compile(ctx, h.store, client, embedder, h.cfg, body.Task, body.Tag)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Read structured JSON if it was written
	var structured map[string]any
	if result.JSONPath != "" {
		if data, err := os.ReadFile(result.JSONPath); err == nil {
			json.Unmarshal(data, &structured)
		}
	}
	var bundle string
	if result.MDPath != "" {
		if data, err := os.ReadFile(result.MDPath); err == nil {
			bundle = string(data)
		}
	}

	jsonResp(w, map[string]any{"structured": structured, "bundle": bundle})
}

// ── Chat (streaming RAG) ───────────────────────────────────────────────────

func (h *handler) apiChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Messages []llm.ChatMessage `json:"messages"`
		Tag      string            `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", 400)
		return
	}
	if len(body.Messages) == 0 {
		jsonError(w, "messages required", 400)
		return
	}

	// Find the latest user message for retrieval
	var query string
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role == "user" {
			query = body.Messages[i].Content
			break
		}
	}
	if query == "" {
		jsonError(w, "no user message found", 400)
		return
	}
	if h.cfg.API.AnthropicAPIKey == "" {
		jsonError(w, "Chat requires an API key. Set ANTHROPIC_API_KEY env var or add it to ~/.context/ctx.toml under [api].", 500)
		return
	}

	client, err := llm.New(h.cfg, h.store)
	if err != nil {
		jsonError(w, "llm init: "+err.Error(), 500)
		return
	}
	ctx := r.Context()

	// Try embed query + retrieve + rank (optional — works without Ollama)
	var knowledgeBlock string
	var ranked []pipeline.RankedItem
	embedder := vectors.New(h.cfg.API.OllamaBaseURL, "all-minilm")
	if queryVec, embedErr := embedder.Embed(ctx, query); embedErr == nil {
		if r, retrieveErr := pipeline.RetrieveAndRank(h.store, queryVec, h.cfg, body.Tag); retrieveErr == nil {
			ranked = r
			knowledgeBlock = pipeline.BuildKnowledgeBlock(ranked, 12)
		}
	}

	// Fallback: use FTS search if embeddings unavailable
	if knowledgeBlock == "" {
		if entries, searchErr := h.store.Search(query + "*"); searchErr == nil && len(entries) > 0 {
			for i, e := range entries {
				if i >= 12 {
					break
				}
				knowledgeBlock += fmt.Sprintf("[%d] %s: %s\n\n", i+1, e.Key, e.Value)
			}
		}
	}

	system := `You are a context synthesis engine for a senior AI/ML engineer.
Given a user question and relevant knowledge pieces retrieved from their past AI coding sessions,
produce a grounded answer.
Be specific, actionable, and grounded in the provided sources.
Do not hallucinate. Only use what is in the provided sources. If the sources do not cover the question, say so directly.
Cite sources inline by referencing their numbered index (e.g. "[1]", "[3]") so the user can trace claims back.

RELEVANT KNOWLEDGE:
` + knowledgeBlock

	// Set up SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported", 500)
		return
	}

	// Send sources first as a metadata event
	sourcesJSON, _ := json.Marshal(pipeline.RankedToSourceList(ranked, 12))
	fmt.Fprintf(w, "event: sources\ndata: %s\n\n", sourcesJSON)
	flusher.Flush()

	// Stream LLM response
	_, _, streamErr := client.Stream(ctx, "compile", body.Messages, system, func(chunk string) {
		// Escape for SSE (newlines become separate data: lines)
		lines := strings.Split(chunk, "\n")
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(w, "data: %s\n", line)
			} else {
				fmt.Fprintf(w, "data: %s\n", line)
			}
		}
		fmt.Fprintf(w, "\n")
		flusher.Flush()
	})

	if streamErr != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", streamErr.Error())
		flusher.Flush()
	}

	fmt.Fprintf(w, "event: done\ndata: {}\n\n")
	flusher.Flush()
}

// ── Synthesize ──────────────────────────────────────────────────────────────

func (h *handler) apiSynthesize(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Sources  []string `json:"sources"`
		Output   string   `json:"output"`
		Type     string   `json:"type"`
		Context  string   `json:"context"`
		Refine   string   `json:"refine"`
		Feedback string   `json:"feedback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", 400)
		return
	}
	if len(body.Sources) == 0 && body.Refine == "" {
		jsonError(w, "sources required", 400)
		return
	}

	client, err := llm.New(h.cfg, h.store)
	if err != nil {
		jsonError(w, "llm init: "+err.Error(), 500)
		return
	}
	embedder := vectors.New(h.cfg.API.OllamaBaseURL, "all-minilm")
	ctx := context.Background()

	result, err := pipeline.Synthesize(ctx, h.store, client, embedder, h.cfg, pipeline.SynthesizeOpts{
		Sources:  body.Sources,
		Output:   body.Output,
		Type:     body.Type,
		Context:  body.Context,
		Refine:   body.Refine,
		Feedback: body.Feedback,
	})
	if err != nil {
		jsonResp(w, map[string]any{"ok": false, "stderr": err.Error()})
		return
	}
	jsonResp(w, map[string]any{"ok": true, "stdout": fmt.Sprintf("Wrote %s (%d chunks)", result.FilePath, result.Chunks)})
}

// ── Candidates ──────────────────────────────────────────────────────────────

func (h *handler) apiCandidates(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	candidates, err := h.store.ListCandidates(status)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	var result []map[string]any
	for _, c := range candidates {
		result = append(result, map[string]any{
			"id": c.ID, "type": c.Type, "slug": c.Slug, "definition": c.Definition,
			"status": c.Status, "frequency": c.Frequency, "source_traces": c.SourceTraces,
			"detected_at": c.DetectedAt, "resolved_at": c.ResolvedAt,
		})
	}
	jsonResp(w, result)
}

func (h *handler) apiCandidateAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	// Parse /api/candidates/{id}/approve or /api/candidates/{id}/dismiss
	path := strings.TrimPrefix(r.URL.Path, "/api/candidates/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		jsonError(w, "invalid path", 400)
		return
	}
	candID, action := parts[0], parts[1]

	switch action {
	case "approve":
		if err := h.store.ApproveCandidate(candID); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		// Write promoted file
		h.promoteCandidate(candID)
		jsonResp(w, map[string]string{"status": "approved"})

	case "dismiss":
		var body struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := h.store.DismissCandidate(candID); err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResp(w, map[string]string{"status": "dismissed"})

	default:
		jsonError(w, "unknown action", 400)
	}
}

func (h *handler) promoteCandidate(candID string) {
	candidates, _ := h.store.ListCandidates("")
	for _, c := range candidates {
		if c.ID != candID {
			continue
		}
		nsMap := map[string]string{
			"skill": "skills/internal", "pattern": "patterns",
			"debugging": "debugging", "primitive": "primitives",
		}
		dirName := nsMap[c.Type]
		if dirName == "" {
			dirName = "patterns"
		}
		targetDir := filepath.Join(h.cfg.ContextDir(), dirName)
		if c.Type == "skill" {
			targetDir = filepath.Join(targetDir, c.Slug)
		}
		os.MkdirAll(targetDir, 0o755)

		filename := c.Slug + ".md"
		if c.Type == "skill" {
			filename = "skill.md"
		}
		targetPath := filepath.Join(targetDir, filename)

		now := time.Now().UTC().Format(time.RFC3339)
		var traceIDs []string
		json.Unmarshal([]byte(c.SourceTraces), &traceIDs)

		nsKey := strings.ReplaceAll(dirName, "/", "_")

		content := fmt.Sprintf("---\nid: %s-%s\nsource: internal\npromoted_from: [%s]\npromoted_at: %s\nfrequency: %d\ntags: [%s]\n---\n\n# %s\n\n%s\n",
			nsKey, c.Slug,
			strings.Join(traceIDs, ", "), now[:10], c.Frequency, c.Slug,
			strings.ReplaceAll(c.Slug, "-", " "), c.Definition)

		os.WriteFile(targetPath, []byte(content), 0o644)

		// Register in knowledge table
		kid := nsKey + "-" + c.Slug
		tagsJSON, _ := json.Marshal([]string{c.Slug})
		h.store.UpsertKnowledge(&store.KnowledgeItem{
			ID: kid, Namespace: nsKey, Slug: c.Slug, FilePath: targetPath,
			Summary: c.Definition, Tags: string(tagsJSON), Source: "internal",
			Frequency: c.Frequency, PromotedAt: now,
		})
		return
	}
}

// ── Namespace ───────────────────────────────────────────────────────────────

func (h *handler) apiNamespace(w http.ResponseWriter, r *http.Request) {
	dirs := []string{"learnings", "candidates", "patterns", "debugging",
		"skills/internal", "skills/external", "primitives", "docs", "plans"}

	var result []map[string]any
	for _, d := range dirs {
		fullDir := filepath.Join(h.cfg.ContextDir(), d)
		var files []map[string]any
		filepath.Walk(fullDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			relPath, _ := filepath.Rel(h.cfg.ContextDir(), path)
			files = append(files, map[string]any{
				"name":  info.Name(),
				"path":  relPath,
				"size":  info.Size(),
				"mtime": info.ModTime().UTC().Format("2006-01-02T15:04"),
			})
			return nil
		})
		result = append(result, map[string]any{
			"namespace": d,
			"count":     len(files),
			"files":     files,
		})
	}
	jsonResp(w, result)
}

// ── File ────────────────────────────────────────────────────────────────────

func (h *handler) apiFile(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		h.apiFilePut(w, r)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonError(w, "path required", 400)
		return
	}
	fullPath := filepath.Join(h.cfg.ContextDir(), path)
	if !h.isValidPath(fullPath) {
		jsonError(w, "not found", 404)
		return
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		jsonError(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func (h *handler) apiFilePut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonError(w, "path and content required", 400)
		return
	}
	fullPath := filepath.Join(h.cfg.ContextDir(), body.Path)
	contextDir, _ := filepath.Abs(h.cfg.ContextDir())
	resolved, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(resolved, contextDir) {
		jsonError(w, "invalid path", 400)
		return
	}
	os.MkdirAll(filepath.Dir(fullPath), 0o755)
	if err := os.WriteFile(fullPath, []byte(body.Content), 0o644); err != nil {
		jsonError(w, "write failed: "+err.Error(), 500)
		return
	}
	jsonResp(w, map[string]string{"status": "ok", "path": body.Path})
}

func (h *handler) apiFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	r.ParseMultipartForm(10 << 20)
	namespace := r.FormValue("namespace")
	validNS := map[string]bool{
		"patterns": true, "debugging": true, "skills/internal": true,
		"skills/external": true, "primitives": true, "docs": true, "plans": true,
	}
	if !validNS[namespace] {
		jsonError(w, "invalid namespace", 400)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonError(w, "file required", 400)
		return
	}
	defer file.Close()

	name := header.Filename
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}

	targetDir := filepath.Join(h.cfg.ContextDir(), namespace)
	os.MkdirAll(targetDir, 0o755)
	targetPath := filepath.Join(targetDir, name)

	data, _ := io.ReadAll(file)
	os.WriteFile(targetPath, data, 0o644)

	relPath, _ := filepath.Rel(h.cfg.ContextDir(), targetPath)
	jsonResp(w, map[string]string{"status": "ok", "path": relPath})
}

func (h *handler) apiFileMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Src == "" || body.Dest == "" {
		jsonError(w, "src and dest required", 400)
		return
	}
	srcFull := filepath.Join(h.cfg.ContextDir(), body.Src)
	destFull := filepath.Join(h.cfg.ContextDir(), body.Dest)
	if !h.isValidPath(srcFull) {
		jsonError(w, "source not found", 404)
		return
	}
	os.MkdirAll(filepath.Dir(destFull), 0o755)
	if err := os.Rename(srcFull, destFull); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResp(w, map[string]string{"status": "ok"})
}

// ── File Tags ───────────────────────────────────────────────────────────────

var tagsPattern = regexp.MustCompile(`(?m)^tags:\s*\[([^\]]*)\]`)

func (h *handler) apiFileTags(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		jsonError(w, "path required", 400)
		return
	}
	fullPath := filepath.Join(h.cfg.ContextDir(), path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		jsonError(w, "not found", 404)
		return
	}
	var tags []string
	if m := tagsPattern.FindSubmatch(data); len(m) > 1 {
		for _, t := range strings.Split(string(m[1]), ",") {
			t = strings.TrimSpace(t)
			t = strings.Trim(t, `"'`)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}
	jsonResp(w, map[string]any{"tags": tags})
}

func (h *handler) apiTagsEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		jsonError(w, "path required", 400)
		return
	}
	fullPath := filepath.Join(h.cfg.ContextDir(), body.Path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		jsonError(w, "not found", 404)
		return
	}
	content := string(data)
	tagsStr := "[" + strings.Join(body.Tags, ", ") + "]"
	if tagsPattern.MatchString(content) {
		content = tagsPattern.ReplaceAllString(content, "tags: "+tagsStr)
	} else if strings.Contains(content, "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			content = "---" + parts[1] + "tags: " + tagsStr + "\n---" + parts[2]
		}
	}
	os.WriteFile(fullPath, []byte(content), 0o644)
	jsonResp(w, map[string]any{"status": "ok", "tags": body.Tags})
}

// ── Graph ───────────────────────────────────────────────────────────────────

func (h *handler) apiGraph(w http.ResponseWriter, r *http.Request) {
	knowledge, _ := h.store.ListKnowledge()
	summaries, _ := h.store.AllSummaries()

	lines := []string{"graph LR"}
	lines = append(lines, `    classDef skill fill:#F5F5F0,stroke:#E63946,stroke-width:2px,color:#1A1A1A`)
	lines = append(lines, `    classDef pattern fill:#F5F5F0,stroke:#1A1A1A,stroke-width:2px,color:#1A1A1A`)
	lines = append(lines, `    classDef debugging fill:#F5F5F0,stroke:#8A8A82,stroke-width:2px,color:#1A1A1A`)
	lines = append(lines, `    classDef primitive fill:#F5F5F0,stroke:#D4D4CC,stroke-width:2px,color:#1A1A1A`)

	nodeIDs := make(map[string]bool)
	nsClass := map[string]string{
		"skill_internal": "skill", "skill_external": "skill",
		"skills/internal": "skill", "skills/external": "skill",
		"pattern": "pattern", "patterns": "pattern",
		"debugging": "debugging", "primitive": "primitive",
	}
	for _, k := range knowledge {
		nid := strings.ReplaceAll(k.Slug, "-", "_")
		if !nodeIDs[nid] {
			cls := nsClass[k.Namespace]
			if cls == "" {
				cls = "pattern"
			}
			lines = append(lines, fmt.Sprintf(`    %s["%s"]:::%s`, nid, k.Slug, cls))
			nodeIDs[nid] = true
		}
	}

	// Co-occurrence edges from summaries
	cooccur := make(map[[2]string]int)
	for _, sm := range summaries {
		var skills []string
		json.Unmarshal([]byte(sm.Skills), &skills)
		for i := range skills {
			skills[i] = strings.TrimSpace(strings.ToLower(skills[i]))
		}
		for i, a := range skills {
			for _, b := range skills[i+1:] {
				pair := [2]string{a, b}
				if a > b {
					pair = [2]string{b, a}
				}
				cooccur[pair]++
			}
		}
	}
	for pair, count := range cooccur {
		if count < 2 {
			continue
		}
		for _, x := range pair {
			nid := strings.ReplaceAll(x, "-", "_")
			if !nodeIDs[nid] {
				lines = append(lines, fmt.Sprintf(`    %s["%s"]:::skill`, nid, x))
				nodeIDs[nid] = true
			}
		}
		lines = append(lines, fmt.Sprintf("    %s --- |%dx| %s",
			strings.ReplaceAll(pair[0], "-", "_"), count,
			strings.ReplaceAll(pair[1], "-", "_")))
	}

	if len(nodeIDs) == 0 {
		lines = append(lines, `    empty["No knowledge yet"]`)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, strings.Join(lines, "\n"))
}

// ── Knowledge ───────────────────────────────────────────────────────────────

func (h *handler) apiKnowledge(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListKnowledge()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	var result []map[string]any
	for _, k := range items {
		result = append(result, map[string]any{
			"id": k.ID, "namespace": k.Namespace, "slug": k.Slug,
			"file_path": k.FilePath, "summary": k.Summary, "tags": k.Tags,
			"frequency": k.Frequency, "promoted_at": k.PromotedAt,
		})
	}
	jsonResp(w, result)
}

// ── Usage ───────────────────────────────────────────────────────────────────

func (h *handler) apiUsage(w http.ResponseWriter, r *http.Request) {
	recent, _ := h.store.RecentUsage(100)
	var rows []map[string]any
	totalCost := 0.0
	totalInput := 0
	totalOutput := 0
	byTask := make(map[string]map[string]any)
	byDay := make(map[string]map[string]any)

	for _, u := range recent {
		rows = append(rows, map[string]any{
			"id": u.ID, "ts": u.TS, "task": u.Task, "model": u.Model,
			"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
			"cost_usd": u.CostUSD, "trace_id": u.TraceID,
		})
		totalCost += u.CostUSD
		totalInput += u.InputTokens
		totalOutput += u.OutputTokens

		if _, ok := byTask[u.Task]; !ok {
			byTask[u.Task] = map[string]any{"calls": 0, "input_tokens": 0, "output_tokens": 0, "cost_usd": 0.0}
		}
		bt := byTask[u.Task]
		bt["calls"] = bt["calls"].(int) + 1
		bt["input_tokens"] = bt["input_tokens"].(int) + u.InputTokens
		bt["output_tokens"] = bt["output_tokens"].(int) + u.OutputTokens
		bt["cost_usd"] = bt["cost_usd"].(float64) + u.CostUSD

		day := ""
		if len(u.TS) >= 10 {
			day = u.TS[:10]
		}
		if day != "" {
			if _, ok := byDay[day]; !ok {
				byDay[day] = map[string]any{"calls": 0, "cost_usd": 0.0, "input_tokens": 0, "output_tokens": 0}
			}
			bd := byDay[day]
			bd["calls"] = bd["calls"].(int) + 1
			bd["cost_usd"] = bd["cost_usd"].(float64) + u.CostUSD
			bd["input_tokens"] = bd["input_tokens"].(int) + u.InputTokens
			bd["output_tokens"] = bd["output_tokens"].(int) + u.OutputTokens
		}
	}

	jsonResp(w, map[string]any{
		"rows": rows,
		"summary": map[string]any{
			"total_cost": totalCost, "total_input": totalInput,
			"total_output": totalOutput, "by_task": byTask, "by_day": byDay,
		},
	})
}

// ── Delete ──────────────────────────────────────────────────────────────────

func (h *handler) apiDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		TraceID string `json:"trace_id"`
		Path    string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", 400)
		return
	}

	var deleted []string
	if body.TraceID != "" {
		h.store.DeleteTrace(body.TraceID)
		deleted = append(deleted, "db:"+body.TraceID)
		// Delete learning file
		learnDir := filepath.Join(h.cfg.ContextDir(), "learnings")
		entries, _ := os.ReadDir(learnDir)
		needle := "id: " + body.TraceID
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(learnDir, entry.Name())
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data[:min(500, len(data))]), needle) {
				os.Remove(path)
				deleted = append(deleted, "file:"+entry.Name())
				break
			}
		}
	}
	if body.Path != "" {
		fullPath := filepath.Join(h.cfg.ContextDir(), body.Path)
		if h.isValidPath(fullPath) {
			os.Remove(fullPath)
			deleted = append(deleted, "file:"+body.Path)
		}
		// Clean up DB entries
		stem := strings.TrimSuffix(filepath.Base(body.Path), ".md")
		parentName := filepath.Base(filepath.Dir(body.Path))
		sourceIDs := []string{
			"ns-" + strings.ReplaceAll(body.Path, "/", "-"),
			"synth-skill_external-" + parentName,
			"synth-skill_internal-" + parentName,
			"synth-pattern-" + parentName,
			"ns-" + strings.ReplaceAll(filepath.Dir(body.Path), "/", "-") + "-" + stem,
		}
		for _, sid := range sourceIDs {
			h.store.DeleteChunksBySource(sid)
			h.store.DeleteKnowledge(sid)
		}
	}
	jsonResp(w, map[string]any{"deleted": deleted})
}

// ── Search ─────────────────────────────────────────────────────────────────

func (h *handler) apiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		jsonResp(w, []any{})
		return
	}

	// FTS search across entries (content search)
	ftsQuery := q + "*" // prefix match for FTS5
	entries, err := h.store.Search(ftsQuery)

	var results []map[string]any

	if err == nil {
		for _, e := range entries {
			if len(results) >= 15 {
				break
			}
			results = append(results, map[string]any{
				"key":    e.Key,
				"path":   pipeline.KeyToPath(e.Key),
				"source": "content",
			})
		}
	}

	// Also search namespace files by path (client might miss these if not in entries table)
	dirs := []string{"learnings", "candidates", "patterns", "debugging",
		"skills/internal", "skills/external", "primitives", "docs", "plans"}
	qLower := strings.ToLower(q)
	seen := make(map[string]bool)
	for _, r := range results {
		seen[r["path"].(string)] = true
	}

	for _, d := range dirs {
		fullDir := filepath.Join(h.cfg.ContextDir(), d)
		filepath.Walk(fullDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			relPath, _ := filepath.Rel(h.cfg.ContextDir(), path)
			if seen[relPath] {
				return nil
			}
			dotKey := strings.TrimSuffix(relPath, ".md")
			dotKey = strings.ReplaceAll(dotKey, "/", ".")
			nameLower := strings.ToLower(info.Name())
			dotKeyLower := strings.ToLower(dotKey)

			if strings.Contains(dotKeyLower, qLower) || strings.Contains(nameLower, qLower) {
				results = append(results, map[string]any{
					"key":    dotKey,
					"path":   relPath,
					"source": "path",
				})
				seen[relPath] = true
			} else if len(results) < 15 {
				// Search file content
				data, readErr := os.ReadFile(path)
				if readErr == nil && strings.Contains(strings.ToLower(string(data)), qLower) {
					results = append(results, map[string]any{
						"key":    dotKey,
						"path":   relPath,
						"source": "content",
					})
					seen[relPath] = true
				}
			}
			return nil
		})
	}

	// Cap at 15 results
	if len(results) > 15 {
		results = results[:15]
	}

	jsonResp(w, results)
}

// ── Park ───────────────────────────────────────────────────────────────────

func (h *handler) apiPark(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Key     string `json:"key"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		jsonError(w, "key required", 400)
		return
	}
	if body.Content == "" {
		jsonError(w, "content required", 400)
		return
	}

	// Use pipeline.SetKey which writes file + entries + knowledge + chunks
	ctx := context.Background()
	var embedder *vectors.Embedder
	if h.cfg.API.OllamaBaseURL != "" {
		embedder = vectors.New(h.cfg.API.OllamaBaseURL, "all-minilm")
	}
	if err := pipeline.SetKey(ctx, h.store, embedder, h.cfg, body.Key, body.Content, "text/markdown"); err != nil {
		jsonError(w, "park failed: "+err.Error(), 500)
		return
	}

	filePath := pipeline.KeyToPath(body.Key)
	jsonResp(w, map[string]string{"status": "ok", "path": filePath})
}

// ── Skills ─────────────────────────────────────────────────────────────────

func (h *handler) apiSkills(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListKnowledge()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}

	// Also scan filesystem for skill files not yet in DB
	claudeSkillsDir := filepath.Join(os.Getenv("HOME"), ".claude", "skills")
	deployedSlugs := make(map[string]bool)
	if entries, err := os.ReadDir(claudeSkillsDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				deployedSlugs[strings.TrimSuffix(e.Name(), ".md")] = true
			}
		}
	}

	var result []map[string]any
	seen := make(map[string]bool)
	for _, k := range items {
		if !strings.Contains(k.Namespace, "skill") {
			continue
		}
		seen[k.Slug] = true
		result = append(result, map[string]any{
			"slug":      k.Slug,
			"namespace": k.Namespace,
			"file_path": k.FilePath,
			"summary":   k.Summary,
			"deployed":  deployedSlugs[k.Slug],
			"frequency": k.Frequency,
		})
	}

	// Also check filesystem skill dirs directly
	for _, dir := range []string{"skills/internal", "skills/external"} {
		fullDir := filepath.Join(h.cfg.ContextDir(), dir)
		filepath.Walk(fullDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			slug := strings.TrimSuffix(info.Name(), ".md")
			if info.Name() == "skill.md" {
				slug = filepath.Base(filepath.Dir(path))
			}
			if seen[slug] {
				return nil
			}
			seen[slug] = true
			relPath, _ := filepath.Rel(h.cfg.ContextDir(), path)
			result = append(result, map[string]any{
				"slug":      slug,
				"namespace": dir,
				"file_path": relPath,
				"summary":   "",
				"deployed":  deployedSlugs[slug],
				"frequency": 0,
			})
			return nil
		})
	}

	jsonResp(w, result)
}

func (h *handler) apiSkillDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Slug     string `json:"slug"`
		FilePath string `json:"file_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Slug == "" || body.FilePath == "" {
		jsonError(w, "slug and file_path required", 400)
		return
	}

	// Read source skill file
	srcPath := filepath.Join(h.cfg.ContextDir(), body.FilePath)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		jsonError(w, "skill file not found: "+err.Error(), 404)
		return
	}

	// Write to ~/.claude/skills/
	claudeSkillsDir := filepath.Join(os.Getenv("HOME"), ".claude", "skills")
	os.MkdirAll(claudeSkillsDir, 0o755)
	destPath := filepath.Join(claudeSkillsDir, body.Slug+".md")
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		jsonError(w, "deploy failed: "+err.Error(), 500)
		return
	}

	jsonResp(w, map[string]string{"status": "ok", "deployed_to": destPath})
}

func (h *handler) apiSkillUndeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		jsonError(w, "POST required", 405)
		return
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Slug == "" {
		jsonError(w, "slug required", 400)
		return
	}

	destPath := filepath.Join(os.Getenv("HOME"), ".claude", "skills", body.Slug+".md")
	if err := os.Remove(destPath); err != nil {
		jsonError(w, "undeploy failed: "+err.Error(), 500)
		return
	}
	jsonResp(w, map[string]string{"status": "ok"})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func (h *handler) isValidPath(fullPath string) bool {
	resolved, err := filepath.Abs(fullPath)
	if err != nil {
		return false
	}
	contextDir, _ := filepath.Abs(h.cfg.ContextDir())
	if !strings.HasPrefix(resolved, contextDir) {
		return false
	}
	_, err = os.Stat(fullPath)
	return err == nil
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
