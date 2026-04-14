package pipeline

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session represents a discoverable session that can be ingested.
type Session struct {
	ID        string // conversation ID or file stem
	Source    string // "cursor-raw", "claude-code", "cursor-transcript"
	Path      string // file or directory path
	Project   string // derived project name
	Timestamp time.Time
	Ingested  bool // true if a trace already exists for this
}

// DiscoverSessions scans known locations for sessions that can be ingested.
// It checks:
//   - ~/.context/raw/ for Cursor hook events (grouped by conversation ID)
//   - Claude Code JSONL files in the configured claude_dir
//   - Cursor agent-transcripts/*.txt in the configured cursor_dir
func DiscoverSessions(contextDir, cursorDir, claudeDir string, ingestedIDs map[string]bool) ([]Session, error) {
	var sessions []Session

	// 1. Cursor raw/ hook events
	rawDir := filepath.Join(contextDir, "raw")
	if raw, err := discoverCursorRaw(rawDir, ingestedIDs); err == nil {
		sessions = append(sessions, raw...)
	}

	// 2. Claude Code JSONL sessions
	if cc, err := discoverClaudeCode(claudeDir, ingestedIDs); err == nil {
		sessions = append(sessions, cc...)
	}

	// 3. Cursor transcripts
	if ct, err := discoverCursorTranscripts(cursorDir, ingestedIDs); err == nil {
		sessions = append(sessions, ct...)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Timestamp.After(sessions[j].Timestamp)
	})

	return sessions, nil
}

func discoverCursorRaw(rawDir string, ingestedIDs map[string]bool) ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(rawDir, "*.json"))
	if err != nil {
		return nil, err
	}

	// Group by conversation ID (prefix before first _)
	convIDs := make(map[string]time.Time)
	for _, f := range files {
		base := filepath.Base(f)
		idx := strings.Index(base, "_")
		if idx < 0 {
			continue
		}
		convID := base[:idx]
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		if t, ok := convIDs[convID]; !ok || info.ModTime().After(t) {
			convIDs[convID] = info.ModTime()
		}
	}

	var sessions []Session
	for convID, ts := range convIDs {
		traceID := "trace-" + convID[:min(8, len(convID))]
		sessions = append(sessions, Session{
			ID:        convID,
			Source:    "cursor-raw",
			Path:      rawDir,
			Timestamp: ts,
			Ingested:  ingestedIDs[traceID],
		})
	}
	return sessions, nil
}

func discoverClaudeCode(claudeDir string, ingestedIDs map[string]bool) ([]Session, error) {
	if claudeDir == "" {
		return nil, nil
	}

	var sessions []Session
	err := filepath.Walk(claudeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		traceID := traceIDFromPath(path)
		sessions = append(sessions, Session{
			ID:        strings.TrimSuffix(info.Name(), ".jsonl"),
			Source:    "claude-code",
			Path:      path,
			Project:   projectFromClaudePath(path),
			Timestamp: info.ModTime(),
			Ingested:  ingestedIDs[traceID],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func discoverCursorTranscripts(cursorDir string, ingestedIDs map[string]bool) ([]Session, error) {
	if cursorDir == "" {
		return nil, nil
	}

	var sessions []Session
	pattern := filepath.Join(cursorDir, "*", "agent-transcripts", "*.txt")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		traceID := traceIDFromPath(f)
		sessions = append(sessions, Session{
			ID:        strings.TrimSuffix(info.Name(), ".txt"),
			Source:    "cursor-transcript",
			Path:      f,
			Project:   projectFromTranscriptPath(f),
			Timestamp: info.ModTime(),
			Ingested:  ingestedIDs[traceID],
		})
	}
	return sessions, nil
}

func traceIDFromPath(path string) string {
	hash := md5.Sum([]byte(path))
	return "trace-" + fmt.Sprintf("%x", hash)[:8]
}

func projectFromClaudePath(path string) string {
	// Claude Code paths: ~/.claude/projects/<project-slug>/sessions/<id>.jsonl
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "sessions" {
		return filepath.Base(filepath.Dir(dir))
	}
	return filepath.Base(dir)
}

func projectFromTranscriptPath(path string) string {
	// Cursor paths: ~/.cursor/projects/<workspace-dir>/agent-transcripts/<id>.txt
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "agent-transcripts" {
		return filepath.Base(filepath.Dir(dir))
	}
	return ""
}
