package pipeline

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
)

// BackfillOpts configures a backfill run.
type BackfillOpts struct {
	DryRun bool
	Limit  int    // max sessions to ingest (0 = unlimited)
	After  string // only sessions after this date (YYYY-MM-DD)
}

// BackfillResult holds stats from a backfill run.
type BackfillResult struct {
	Discovered  int
	Ingested    int
	Summarized  int
	Embedded    int
}

// Backfill composes discover → ingest → summarize → index for unprocessed sessions.
func Backfill(ctx context.Context, s *store.Store, client *llm.Client, embedder *vectors.Embedder, cfg *config.Config, opts BackfillOpts) (*BackfillResult, error) {
	result := &BackfillResult{}

	// Build set of existing trace IDs
	traces, err := s.ListTraces()
	if err != nil {
		return nil, err
	}
	ingestedIDs := make(map[string]bool)
	for _, t := range traces {
		ingestedIDs[t.ID] = true
	}

	// Discover
	sessions, err := DiscoverSessions(cfg.ContextDir(), cfg.Capture.CursorDir, cfg.Capture.ClaudeDir, ingestedIDs)
	if err != nil {
		return nil, err
	}
	result.Discovered = len(sessions)

	// Filter to un-ingested, optionally by date
	var afterTime time.Time
	if opts.After != "" {
		afterTime, _ = time.Parse("2006-01-02", opts.After)
	}

	count := 0
	for _, sess := range sessions {
		if sess.Ingested {
			continue
		}
		if !afterTime.IsZero() && sess.Timestamp.Before(afterTime) {
			continue
		}
		if opts.Limit > 0 && count >= opts.Limit {
			break
		}

		if opts.DryRun {
			proj := ""
			if sess.Project != "" {
				proj = fmt.Sprintf(" (%s)", sess.Project)
			}
			fmt.Printf("  [ingest] %s %s%s\n", sess.Source, sess.ID[:min(20, len(sess.ID))], proj)
			count++
			continue
		}

		// Ingest based on source type
		var ingestErr error
		switch sess.Source {
		case "claude-code":
			_, ingestErr = IngestClaudeCode(s, cfg, sess.Path)
		case "cursor-transcript":
			_, ingestErr = IngestCursorTranscript(s, cfg, sess.Path, "", sess.Project)
		case "cursor-raw":
			_, ingestErr = IngestCursorRaw(s, cfg, sess.ID)
		}
		if ingestErr != nil {
			fmt.Fprintf(os.Stderr, "  SKIP %s: %v\n", sess.ID, ingestErr)
			continue
		}
		result.Ingested++
		count++
	}

	if opts.DryRun {
		result.Ingested = count
		return result, nil
	}

	// Summarize unsummarized
	if client != nil {
		sumResult, err := SummarizeAll(ctx, s, client, cfg, false)
		if err == nil {
			result.Summarized = sumResult.Succeeded
		}
	}

	// Embed unembedded
	if embedder != nil {
		idxResult, err := IndexAll(ctx, s, embedder, cfg, false)
		if err == nil {
			result.Embedded = idxResult.TracesEmbedded + idxResult.ChunksCreated
		}
	}

	return result, nil
}
