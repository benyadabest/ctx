package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/pipeline"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/fsnotify/fsnotify"
)

// Watch monitors the Claude Code sessions directory for new JSONL files
// and auto-ingests them.
func Watch(ctx context.Context, s *store.Store, cfg *config.Config) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close()

	claudeDir := cfg.Capture.ClaudeDir
	if claudeDir == "" {
		return fmt.Errorf("claude_dir not configured")
	}

	// Walk and watch all project subdirectories
	watchDirs := 0
	filepath.Walk(claudeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			watcher.Add(path)
			watchDirs++
		}
		return nil
	})

	fmt.Printf("ctx-watch: monitoring %d directories under %s\n", watchDirs, claudeDir)
	fmt.Println("Press Ctrl+C to stop.")

	// Debounce: track recently ingested files to avoid double-processing
	recentlyIngested := make(map[string]time.Time)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !isRelevantEvent(event) {
				continue
			}
			if !strings.HasSuffix(event.Name, ".jsonl") {
				continue
			}

			// Debounce: skip if ingested in last 5 seconds
			if t, ok := recentlyIngested[event.Name]; ok && time.Since(t) < 5*time.Second {
				continue
			}

			// Wait briefly for file to finish writing
			time.Sleep(500 * time.Millisecond)

			result, err := pipeline.IngestClaudeCode(s, cfg, event.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ctx-watch: error ingesting %s: %v\n", filepath.Base(event.Name), err)
				continue
			}

			recentlyIngested[event.Name] = time.Now()
			fmt.Printf("ctx-watch: ingested %s → %s\n", filepath.Base(event.Name), result.TraceID)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "ctx-watch: error: %v\n", err)
		}
	}
}

func isRelevantEvent(event fsnotify.Event) bool {
	return event.Op&(fsnotify.Create|fsnotify.Write) != 0
}
