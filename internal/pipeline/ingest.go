package pipeline

import (
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
)

// IngestResult holds what was produced by an ingest operation.
type IngestResult struct {
	TraceID  string
	LearnMD  string // path to written .md file
	IsNew    bool
}

// IngestCursorRaw ingests Cursor hook events from the raw/ directory for a conversation.
func IngestCursorRaw(s *store.Store, cfg *config.Config, conversationID string) (*IngestResult, error) {
	rawDir := filepath.Join(cfg.ContextDir(), "raw")
	pattern := filepath.Join(rawDir, conversationID+"_*.json")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob raw files: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no raw files found for conversation_id=%s", conversationID)
	}
	sort.Strings(files)

	var (
		prompt       string
		filesEdited  []string
		status       = "unknown"
		loopCount    int
		workspace    string
		ts           = time.Now().UTC().Format(time.RFC3339)
	)

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}

		event, _ := obj["hook_event_name"].(string)
		switch event {
		case "beforeSubmitPrompt":
			if p, ok := obj["prompt"].(string); ok {
				prompt = p
			}
			workspace = firstWorkspaceRoot(obj)
			if t, ok := obj["ts"].(string); ok {
				ts = t
			}
		case "afterFileEdit":
			if fp, ok := obj["file_path"].(string); ok && fp != "" {
				if !contains(filesEdited, fp) {
					filesEdited = append(filesEdited, fp)
				}
			}
			if workspace == "" {
				workspace = firstWorkspaceRoot(obj)
			}
		case "stop":
			if st, ok := obj["status"].(string); ok {
				status = st
			}
			if lc, ok := obj["loop_count"].(float64); ok {
				loopCount = int(lc)
			}
			if workspace == "" {
				workspace = firstWorkspaceRoot(obj)
			}
		}
	}

	project := ""
	if workspace != "" {
		project = filepath.Base(workspace)
	}
	traceID := "trace-" + conversationID[:min(8, len(conversationID))]

	filesJSON, _ := json.Marshal(filesEdited)

	learnPath, err := writeLearningMD(cfg, writeLearningParams{
		traceID:        traceID,
		conversationID: conversationID,
		ts:             ts,
		source:         "cursor",
		status:         status,
		workspace:      workspace,
		project:        project,
		prompt:         prompt,
		filesModified:  filesEdited,
		loopCount:      loopCount,
	})
	if err != nil {
		return nil, err
	}

	err = s.UpsertTrace(&store.Trace{
		ID:             traceID,
		ConversationID: conversationID,
		TS:             ts,
		Source:         "cursor",
		Status:         status,
		Workspace:      workspace,
		Project:        project,
		Prompt:         prompt,
		FilesModified:  string(filesJSON),
		LoopCount:      loopCount,
		RawPath:        rawDir,
	})
	if err != nil {
		return nil, err
	}

	return &IngestResult{TraceID: traceID, LearnMD: learnPath, IsNew: true}, nil
}

// IngestClaudeCode ingests a Claude Code JSONL session file.
func IngestClaudeCode(s *store.Store, cfg *config.Config, jsonlPath string) (*IngestResult, error) {
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", jsonlPath, err)
	}

	var entries []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		entries = append(entries, obj)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("empty or unparseable JSONL: %s", jsonlPath)
	}

	first := entries[0]
	sessionID, _ := first["sessionId"].(string)
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(jsonlPath), filepath.Ext(jsonlPath))
	}
	workspace, _ := first["cwd"].(string)
	project := ""
	if workspace != "" {
		project = filepath.Base(workspace)
	}

	ts, _ := first["timestamp"].(string)
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	var userMessages, assistantMessages []string
	for _, entry := range entries {
		etype, _ := entry["type"].(string)
		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}

		switch etype {
		case "user":
			userMessages = append(userMessages, extractTextContent(msg)...)
		case "assistant":
			assistantMessages = append(assistantMessages, extractTextContent(msg)...)
		}
	}

	// Stable trace ID from MD5 of file path (matches Python)
	hash := md5.Sum([]byte(jsonlPath))
	pathHash := fmt.Sprintf("%x", hash)[:8]
	traceID := "trace-" + pathHash

	prompt := ""
	if len(userMessages) > 0 {
		prompt = userMessages[0]
	}

	dtStr := ts[:min(10, len(ts))]
	learnName := fmt.Sprintf("claude-trace-%s-%s.md", dtStr, pathHash)

	extraContent := formatClaudeCodeBody(userMessages, assistantMessages)

	learnPath, err := writeLearningMD(cfg, writeLearningParams{
		traceID:        traceID,
		conversationID: sessionID,
		ts:             ts,
		source:         "claude-code",
		status:         "completed",
		workspace:      workspace,
		project:        project,
		prompt:         prompt,
		filesModified:  nil,
		loopCount:      len(entries),
		learnName:      learnName,
		extraContent:   extraContent,
	})
	if err != nil {
		return nil, err
	}

	filesJSON, _ := json.Marshal([]string{})
	err = s.UpsertTrace(&store.Trace{
		ID:             traceID,
		ConversationID: sessionID,
		TS:             ts,
		Source:         "claude-code",
		Status:         "completed",
		Workspace:      workspace,
		Project:        project,
		Prompt:         prompt,
		FilesModified:  string(filesJSON),
		LoopCount:      len(entries),
		RawPath:        jsonlPath,
	})
	if err != nil {
		return nil, err
	}

	return &IngestResult{TraceID: traceID, LearnMD: learnPath, IsNew: true}, nil
}

// IngestCursorTranscript ingests a Cursor agent-transcripts/*.txt file.
func IngestCursorTranscript(s *store.Store, cfg *config.Config, txtPath, workspaceHint, projectHint string) (*IngestResult, error) {
	content, err := os.ReadFile(txtPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", txtPath, err)
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return nil, fmt.Errorf("empty transcript: %s", txtPath)
	}

	hash := md5.Sum([]byte(txtPath))
	pathHash := fmt.Sprintf("%x", hash)[:8]
	traceID := "trace-" + pathHash

	lines := strings.Split(string(content), "\n")

	// Extract first user prompt
	prompt := extractTranscriptPrompt(lines)

	// Extract mentioned file paths
	filesModified := extractTranscriptFiles(lines)
	if len(filesModified) > 20 {
		filesModified = filesModified[:20]
	}

	// Derive workspace/project
	workspace := workspaceHint
	project := projectHint
	if project == "" {
		parent := filepath.Base(filepath.Dir(filepath.Dir(txtPath)))
		parts := strings.Split(strings.TrimPrefix(parent, "Users-"+os.Getenv("USER")+"-"), "-")
		noise := map[string]bool{"Library": true, "Application": true, "Support": true, "Cursor": true, "Workspaces": true, "workspace": true, "json": true}
		var clean []string
		for _, p := range parts {
			if !noise[p] && !isAllDigits(p) {
				clean = append(clean, p)
			}
		}
		if len(clean) > 0 {
			project = clean[len(clean)-1]
		} else if len(parent) > 20 {
			project = parent[:20]
		} else {
			project = parent
		}
	}

	// Timestamp from file mtime
	ts := time.Now().UTC().Format(time.RFC3339)
	if info, err := os.Stat(txtPath); err == nil {
		ts = info.ModTime().UTC().Format(time.RFC3339)
	}

	conversationID := strings.TrimSuffix(filepath.Base(txtPath), filepath.Ext(txtPath))
	dtStr := ts[:min(10, len(ts))]
	learnName := fmt.Sprintf("cursor-trace-%s-%s.md", dtStr, pathHash)

	loopCount := strings.Count(string(content), "\nassistant:")

	filesJSON, _ := json.Marshal(filesModified)

	learnPath, err := writeLearningMD(cfg, writeLearningParams{
		traceID:        traceID,
		conversationID: conversationID,
		ts:             ts,
		source:         "cursor",
		status:         "completed",
		workspace:      workspace,
		project:        project,
		prompt:         prompt,
		filesModified:  filesModified,
		loopCount:      loopCount,
		learnName:      learnName,
	})
	if err != nil {
		return nil, err
	}

	err = s.UpsertTrace(&store.Trace{
		ID:             traceID,
		ConversationID: conversationID,
		TS:             ts,
		Source:         "cursor",
		Status:         "completed",
		Workspace:      workspace,
		Project:        project,
		Prompt:         prompt,
		FilesModified:  string(filesJSON),
		LoopCount:      loopCount,
		RawPath:        txtPath,
	})
	if err != nil {
		return nil, err
	}

	return &IngestResult{TraceID: traceID, LearnMD: learnPath, IsNew: true}, nil
}

// ── Shared helpers ──────────────────────────────────────────────────────────

type writeLearningParams struct {
	traceID        string
	conversationID string
	ts             string
	source         string
	status         string
	workspace      string
	project        string
	prompt         string
	filesModified  []string
	loopCount      int
	learnName      string
	extraContent   string
}

func writeLearningMD(cfg *config.Config, p writeLearningParams) (string, error) {
	learnDir := filepath.Join(cfg.ContextDir(), "learnings")
	if err := os.MkdirAll(learnDir, 0o755); err != nil {
		return "", fmt.Errorf("create learnings dir: %w", err)
	}

	name := p.learnName
	if name == "" {
		dtStr := p.ts[:min(10, len(p.ts))]
		short := p.conversationID
		if len(short) > 8 {
			short = short[:8]
		}
		if short == "" {
			short = p.traceID[len(p.traceID)-8:]
		}
		name = fmt.Sprintf("cursor-trace-%s-%s.md", dtStr, short)
	}

	outPath := filepath.Join(learnDir, name)

	tags := []string{p.source + "-trace", p.status}

	var buf strings.Builder
	fmt.Fprintf(&buf, "---\nid: %s\nconversation_id: %s\nts: %s\nsource: %s\nstatus: %s\nworkspace: %s\nproject: %s\ntags: [%s]\n---\n\n",
		p.traceID, p.conversationID, p.ts, p.source, p.status, p.workspace, p.project, strings.Join(tags, ", "))

	if p.prompt != "" {
		fmt.Fprintf(&buf, "## Prompt\n\n%s\n\n", p.prompt)
	}
	if len(p.filesModified) > 0 {
		buf.WriteString("## Files Modified\n\n")
		for _, fp := range p.filesModified {
			fmt.Fprintf(&buf, "- %s\n", fp)
		}
		buf.WriteString("\n")
	}
	fmt.Fprintf(&buf, "## Session Info\n\nLoop count: %d  \nStatus: %s\n", p.loopCount, p.status)
	if p.extraContent != "" {
		fmt.Fprintf(&buf, "\n%s\n", p.extraContent)
	}

	if err := os.WriteFile(outPath, []byte(buf.String()), 0o644); err != nil {
		return "", fmt.Errorf("write learning: %w", err)
	}
	return outPath, nil
}

func extractTextContent(msg map[string]any) []string {
	var texts []string
	content := msg["content"]
	switch c := content.(type) {
	case string:
		if t := strings.TrimSpace(c); t != "" {
			texts = append(texts, t)
		}
	case []any:
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if typ, _ := m["type"].(string); typ == "text" {
					if t, _ := m["text"].(string); strings.TrimSpace(t) != "" {
						texts = append(texts, strings.TrimSpace(t))
					}
				}
			}
		}
	}
	return texts
}

func formatClaudeCodeBody(userMsgs, assistantMsgs []string) string {
	var parts []string
	if len(userMsgs) > 0 {
		parts = append(parts, "## User Messages\n")
		limit := len(userMsgs)
		if limit > 5 {
			limit = 5
		}
		for i, m := range userMsgs[:limit] {
			text := m
			if len(text) > 500 {
				text = text[:500]
			}
			parts = append(parts, fmt.Sprintf("**[%d]** %s\n", i+1, text))
		}
	}
	if len(assistantMsgs) > 0 {
		parts = append(parts, "\n## Assistant Summary\n")
		text := assistantMsgs[0]
		if len(text) > 800 {
			text = text[:800]
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

func extractTranscriptPrompt(lines []string) string {
	// Try <user_query> tag first
	for i, line := range lines {
		if strings.Contains(line, "<user_query>") && i+1 < len(lines) {
			var queryLines []string
			for j := i + 1; j < len(lines); j++ {
				if strings.Contains(lines[j], "</user_query>") {
					break
				}
				queryLines = append(queryLines, strings.TrimSpace(lines[j]))
			}
			if len(queryLines) > 0 {
				return strings.Join(queryLines, " ")
			}
		}
	}

	// Fall back to first user: block
	var promptLines []string
	inUser := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "user:" {
			inUser = true
			continue
		}
		if strings.TrimSpace(line) == "assistant:" && inUser {
			break
		}
		if inUser {
			stripped := strings.TrimSpace(line)
			if strings.HasPrefix(stripped, "<") && strings.HasSuffix(stripped, ">") {
				continue
			}
			if stripped != "" {
				promptLines = append(promptLines, stripped)
			}
		}
	}
	if len(promptLines) > 5 {
		promptLines = promptLines[:5]
	}
	return strings.Join(promptLines, " ")
}

func extractTranscriptFiles(lines []string) []string {
	var files []string
	for _, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "file_path") && !strings.Contains(lower, "path=") {
			continue
		}
		for _, part := range strings.Split(line, `"`) {
			if strings.Contains(part, "/") {
				segments := strings.Split(part, "/")
				last := segments[len(segments)-1]
				if strings.Contains(last, ".") && len(part) < 200 {
					if !contains(files, part) {
						files = append(files, part)
					}
				}
			}
		}
	}
	return files
}

func firstWorkspaceRoot(obj map[string]any) string {
	roots, ok := obj["workspace_roots"].([]any)
	if !ok || len(roots) == 0 {
		return ""
	}
	s, _ := roots[0].(string)
	return s
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
