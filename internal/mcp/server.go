package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/pipeline"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func Serve(s *store.Store, cfg *config.Config) error {
	srv := server.NewMCPServer(
		"ctx",
		"1.0.0",
	)

	srv.AddTool(
		mcp.NewTool("ctx_get",
			mcp.WithDescription("Retrieve a knowledge entry by key. Keys map to namespace files: docs.api-guide → ~/.context/docs/api-guide.md"),
			mcp.WithString("key", mcp.Required(), mcp.Description("Dot-separated key (e.g. docs.api-guide, patterns.retry)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key := req.GetArguments()["key"].(string)
			value, _, err := pipeline.GetKey(s, cfg, key)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(value), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_set",
			mcp.WithDescription("Store a knowledge entry. Writes namespace file + indexes for FTS + registers in knowledge table. Key maps to path: docs.api-guide → ~/.context/docs/api-guide.md"),
			mcp.WithString("key", mcp.Required(), mcp.Description("Dot-separated key (e.g. docs.api-guide, patterns.retry)")),
			mcp.WithString("value", mcp.Required(), mcp.Description("Content to store")),
			mcp.WithString("content_type", mcp.Description("Content type (default: text/markdown)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			key := args["key"].(string)
			value := args["value"].(string)
			contentType := "text/markdown"
			if ct, ok := args["content_type"].(string); ok && ct != "" {
				contentType = ct
			}
			if err := pipeline.SetKey(ctx, s, nil, cfg, key, value, contentType); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("stored: " + key + " → " + pipeline.KeyToPath(key)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_delete",
			mcp.WithDescription("Delete a knowledge entry. Removes namespace file + entries + knowledge + chunks."),
			mcp.WithString("key", mcp.Required(), mcp.Description("Dot-separated key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key := req.GetArguments()["key"].(string)
			if err := pipeline.DeleteKey(s, cfg, key); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("deleted: " + key), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_list",
			mcp.WithDescription("List knowledge keys. Scans namespace files on disk + entries table."),
			mcp.WithString("prefix", mcp.Description("Key prefix filter (e.g. docs, patterns)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			prefix := ""
			if p, ok := req.GetArguments()["prefix"].(string); ok {
				prefix = p
			}
			keys, err := pipeline.ListKeys(s, cfg, prefix)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, _ := json.Marshal(keys)
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_search",
			mcp.WithDescription("Full-text search across all knowledge entries"),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search query (supports AND, OR, prefix*)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query := req.GetArguments()["query"].(string)
			entries, err := s.Search(query)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			type result struct {
				Key         string `json:"key"`
				Value       string `json:"value"`
				ContentType string `json:"content_type"`
			}
			results := make([]result, len(entries))
			for i, e := range entries {
				results[i] = result{Key: e.Key, Value: e.Value, ContentType: e.ContentType}
			}
			b, err := json.Marshal(results)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("marshal: %v", err)), nil
			}
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	// ── Phase 7: Extended MCP tools ─────────────────────────────────────────

	srv.AddTool(
		mcp.NewTool("ctx_compile",
			mcp.WithDescription("Build a context briefing from your knowledge base. Searches traces, chunks, and namespace files to assemble relevant context for a task."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Task description or problem statement to compile context for")),
			mcp.WithString("tag", mcp.Description("Filter by project tag (e.g. ctx, my-app)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			task := args["task"].(string)
			tag := ""
			if t, ok := args["tag"].(string); ok {
				tag = t
			}

			client, err := llm.New(cfg, s)
			if err != nil {
				return mcp.NewToolResultError("llm init: " + err.Error()), nil
			}
			embedder := vectors.New(cfg.API.OllamaBaseURL, "all-minilm")

			result, err := pipeline.Compile(ctx, s, client, embedder, cfg, task, tag)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			// Return the markdown bundle (most useful for LLM consumers)
			if result.MDPath != "" {
				if data, err := os.ReadFile(result.MDPath); err == nil {
					return mcp.NewToolResultText(string(data)), nil
				}
			}
			return mcp.NewToolResultText("Compiled briefing written but could not read output"), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_status",
			mcp.WithDescription("Show pipeline health: table row counts, timestamps, and pending work"),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			stats, err := s.Stats()
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			var sb strings.Builder
			sb.WriteString("ctx — pipeline status\n\nTables:\n")
			for _, name := range []string{"entries", "traces", "summaries", "embeddings", "candidates", "knowledge", "chunks", "usage"} {
				sb.WriteString(fmt.Sprintf("  %-14s %d\n", name, stats[name]))
			}

			sb.WriteString("\nTimestamps:\n")
			for _, key := range []string{"last_summarize_ts", "last_embed_ts", "last_detection_run"} {
				val, _ := s.GetMeta(key)
				if val == "" {
					val = "(never)"
				}
				sb.WriteString(fmt.Sprintf("  %-24s %s\n", key, val))
			}

			unsumm, _ := s.UnsummarizedTraces()
			unemb, _ := s.UnembeddedSummaries()
			sb.WriteString("\nPending:\n")
			sb.WriteString(fmt.Sprintf("  %-24s %d\n", "unsummarized traces", len(unsumm)))
			sb.WriteString(fmt.Sprintf("  %-24s %d\n", "unembedded summaries", len(unemb)))

			return mcp.NewToolResultText(sb.String()), nil
		},
	)

	srv.AddTool(
		mcp.NewTool("ctx_note",
			mcp.WithDescription("Create a quick timestamped note in the learnings namespace"),
			mcp.WithString("text", mcp.Required(), mcp.Description("Note content")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text := req.GetArguments()["text"].(string)

			result, err := pipeline.Note(s, cfg, text)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			return mcp.NewToolResultText("noted: " + result), nil
		},
	)

	return server.ServeStdio(srv)
}
