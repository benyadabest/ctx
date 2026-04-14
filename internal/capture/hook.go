package capture

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// HandleHook reads a Cursor hook JSON event from stdin and writes it to
// ~/.context/raw/<conversation_id>_<timestamp>.json.
// This is the handler for `ctx hook` which Cursor calls on each event.
func HandleHook(contextDir string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}

	convID, _ := event["conversation_id"].(string)
	if convID == "" {
		convID = "unknown"
	}

	rawDir := filepath.Join(contextDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return fmt.Errorf("create raw dir: %w", err)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	filename := fmt.Sprintf("%s_%s.json", convID, ts)
	outPath := filepath.Join(rawDir, filename)

	// Add timestamp to the event
	event["ts"] = time.Now().UTC().Format(time.RFC3339)

	pretty, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.WriteFile(outPath, pretty, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}

	fmt.Fprintf(os.Stderr, "ctx-hook: wrote %s\n", outPath)
	return nil
}
