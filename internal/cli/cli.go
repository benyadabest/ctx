package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/benyadabest/ctx/internal/capture"
	"github.com/benyadabest/ctx/internal/config"
	"github.com/benyadabest/ctx/internal/llm"
	"github.com/benyadabest/ctx/internal/mcp"
	"github.com/benyadabest/ctx/internal/pipeline"
	"github.com/benyadabest/ctx/internal/store"
	"github.com/benyadabest/ctx/internal/vectors"
	"github.com/benyadabest/ctx/internal/web"
	"github.com/spf13/cobra"
)

var (
	dbPath    string
	dataStore *store.Store
)

func defaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".context", "index.db")
}

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ctx",
		Short: "Personal knowledge store",
		Long: `ctx — personal knowledge store

Park and retrieve knowledge from the terminal.
Data lives in ~/.context/index.db (SQLite + FTS5 full-text search).

Keys are dot-separated lowercase segments: react.hooks.rules, go.errors.wrapping

Quick start:
  ctx set react.hooks.rules "Always call hooks at the top level"
  ctx get react.hooks.rules
  ctx set go.errors '{"wrap": true, "sentinel": false}' --json
  ctx list react                  # keys under react.*
  ctx search "hooks"              # full-text search
  ctx tree                        # browse all keys as a tree
  ctx delete react.hooks.rules

Pipe content in:
  echo "long notes" | ctx set my.notes
  cat design.md | ctx set project.design -

MCP server (for Claude Code / Cursor):
  ctx mcp                         # starts stdio server`,
		// Bare `ctx` with no args: show compact help, not the full Long description
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(`ctx — personal knowledge store

Key-Value Store:
  ctx set <key> <value>     Store knowledge
  ctx get <key>             Retrieve it
  ctx search <query>        Full-text search
  ctx list [prefix]         List keys
  ctx tree [prefix]         Browse as tree
  ctx delete <key>          Remove entry

Pipeline:
  ctx discover              List sessions to ingest
  ctx ingest <id>           Ingest a session
  ctx summarize             AI-summarize traces
  ctx embed                 Embed into vectors
  ctx detect                Find recurring patterns
  ctx compile "task"        Build context briefing
  ctx synthesize            Create skills from sources

Knowledge:
  ctx push <file> -n <ns>   Add file/URL to knowledge
  ctx note "text"           Quick note
  ctx tag <file> <tag>      Tag a knowledge item
  ctx forget <id>           Remove knowledge item
  ctx rm <path>             Delete file + DB entries

Operations:
  ctx backfill              Discover + ingest + summarize + embed
  ctx watch                 Auto-ingest new Claude Code sessions
  ctx status                Pipeline health
  ctx serve                 Start web UI (localhost:7337)
  ctx hook                  Handle Cursor hook event
  ctx mcp                   Start MCP server

Run ctx --help for detailed examples, or ctx <command> --help for a specific command.`)
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "help" || cmd.Name() == "completion" || cmd.Name() == "ctx" {
				return nil
			}
			var err error
			dataStore, err = store.New(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			return nil
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if dataStore != nil {
				dataStore.Close()
			}
		},
	}

	root.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "database path")
	root.AddCommand(getCmd(), setCmd(), deleteCmd(), listCmd(), searchCmd(), treeCmd(), mcpCmd())
	root.AddCommand(discoverCmd(), ingestCmd(), hookCmd())
	root.AddCommand(summarizeCmd(), embedCmd())
	root.AddCommand(detectCmd(), compileCmd(), synthesizeCmd())
	root.AddCommand(pushCmd(), noteCmd(), tagCmd(), forgetCmd(), rmCmd(), backfillCmd(), watchCmd(), statusCmd())
	root.AddCommand(serveCmd())
	return root
}

func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Retrieve an entry",
		Long: `Retrieve and print the value of a knowledge entry by its key.
Keys map to namespace files: docs.api-guide → ~/.context/docs/api-guide.md`,
		Example: `  ctx get docs.api-guide
  ctx get patterns.retry-logic
  ctx get react.hooks.rules`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			value, _, err := pipeline.GetKey(dataStore, cfg, args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			fmt.Print(value)
			if !strings.HasSuffix(value, "\n") {
				fmt.Println()
			}
			return nil
		},
	}
}

func setCmd() *cobra.Command {
	var isJSON bool
	cmd := &cobra.Command{
		Use:   "set <key> [value]",
		Short: "Store an entry",
		Long: `Store a knowledge entry. Value can be a positional argument, piped from stdin,
or read from stdin with "-".

Keys map to namespace files: docs.api-guide → ~/.context/docs/api-guide.md
Content is written to disk, indexed for FTS, and registered in the knowledge table.

Keys must be dot-separated lowercase alphanumeric segments (hyphens allowed).
Valid:   docs.api-guide, patterns.retry, skills.internal.error-handling
Invalid: React.Hooks, foo..bar, foo_bar, foo/bar`,
		Example: `  ctx set docs.api-guide "# API Guide\nEndpoints..."
  ctx set patterns.retry "# Retry Pattern\n..."
  cat design.md | ctx set docs.design -
  echo "long content" | ctx set notes.today`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var value string
			if len(args) == 2 {
				if args[1] == "-" {
					b, err := io.ReadAll(os.Stdin)
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
					value = string(b)
				} else {
					value = args[1]
				}
			} else {
				fi, _ := os.Stdin.Stat()
				if fi.Mode()&os.ModeCharDevice == 0 {
					b, err := io.ReadAll(os.Stdin)
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
					value = string(b)
				} else {
					return fmt.Errorf("value required: provide as argument or pipe via stdin")
				}
			}

			value = strings.TrimRight(value, "\n")

			contentType := "text/markdown"
			if isJSON {
				if !json.Valid([]byte(value)) {
					return fmt.Errorf("invalid JSON value")
				}
				contentType = "application/json"
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Try to get embedder (non-fatal if unavailable)
			var embedder *vectors.Embedder
			if cfg.API.OllamaBaseURL != "" {
				embedder = vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			}

			return pipeline.SetKey(context.Background(), dataStore, embedder, cfg, args[0], value, contentType)
		},
	}
	cmd.Flags().BoolVar(&isJSON, "json", false, "validate and store as JSON")
	return cmd
}

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <key>",
		Short: "Delete an entry",
		Long:  "Remove the namespace file, entries table row, knowledge entry, and chunks for a key.",
		Example: `  ctx delete docs.api-guide
  ctx delete patterns.retry`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return pipeline.DeleteKey(dataStore, cfg, args[0])
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [prefix]",
		Short: "List keys",
		Long: `List all keys, or filter by a dot-prefix to see keys under a namespace.
Scans both namespace files on disk and entries table.`,
		Example: `  ctx list                        # all keys
  ctx list docs                   # keys under docs.*
  ctx list patterns               # keys under patterns.*`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}
			keys, err := pipeline.ListKeys(dataStore, cfg, prefix)
			if err != nil {
				return err
			}
			for _, k := range keys {
				fmt.Println(k)
			}
			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search entries",
		Long: `Search entry keys and values using SQLite FTS5 full-text search.
Supports AND, OR, NOT operators and prefix matching with *.`,
		Example: `  ctx search hooks                # find entries mentioning "hooks"
  ctx search "error AND wrap"     # boolean query
  ctx search "react*"             # prefix match`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := dataStore.Search(args[0])
			if err != nil {
				return err
			}
			for _, e := range entries {
				val := e.Value
				if len(val) > 80 {
					val = val[:77] + "..."
				}
				fmt.Printf("%s\t%s\n", e.Key, val)
			}
			return nil
		},
	}
}

func treeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tree [prefix]",
		Short: "Display keys as a tree",
		Long:  "Render keys as an indented tree by splitting on dots. Includes namespace files and entries.",
		Example: `  ctx tree                        # full tree
  ctx tree docs                   # subtree under docs
  ctx tree patterns               # subtree under patterns`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}
			keys, err := pipeline.ListKeys(dataStore, cfg, prefix)
			if err != nil {
				return err
			}

			type node struct {
				children map[string]*node
			}
			root := &node{children: make(map[string]*node)}

			for _, key := range keys {
				parts := strings.Split(key, ".")
				cur := root
				for _, p := range parts {
					if cur.children[p] == nil {
						cur.children[p] = &node{children: make(map[string]*node)}
					}
					cur = cur.children[p]
				}
			}

			var printTree func(n *node, indent string)
			printTree = func(n *node, indent string) {
				childKeys := make([]string, 0, len(n.children))
				for k := range n.children {
					childKeys = append(childKeys, k)
				}
				sort.Strings(childKeys)
				for _, k := range childKeys {
					fmt.Printf("%s%s\n", indent, k)
					printTree(n.children[k], indent+"  ")
				}
			}

			printTree(root, "")
			return nil
		},
	}
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start MCP server (stdio)",
		Long: `Start an MCP (Model Context Protocol) server over stdio.
Used by Claude Code, Cursor, and other MCP-compatible tools.

Add to your MCP config:
  { "mcpServers": { "ctx": { "command": "ctx", "args": ["mcp"] } } }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return mcp.Serve(dataStore, cfg)
		},
	}
}

func loadConfig() (*config.Config, error) {
	return config.Load()
}

func discoverCmd() *cobra.Command {
	var showAll bool
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List available sessions to ingest",
		Long: `Scan known locations for AI coding sessions that can be ingested.

Checks:
  - ~/.context/raw/ for Cursor hook events
  - Claude Code JSONL session files
  - Cursor agent-transcripts/*.txt

Sessions already ingested are marked with [ingested]. Use --all to include them.`,
		Example: `  ctx discover                # show new sessions
  ctx discover --all          # include already-ingested`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Build set of existing trace IDs
			traces, err := dataStore.ListTraces()
			if err != nil {
				return err
			}
			ingestedIDs := make(map[string]bool)
			for _, t := range traces {
				ingestedIDs[t.ID] = true
			}

			sessions, err := pipeline.DiscoverSessions(
				cfg.ContextDir(), cfg.Capture.CursorDir, cfg.Capture.ClaudeDir, ingestedIDs,
			)
			if err != nil {
				return err
			}

			count := 0
			for _, s := range sessions {
				if s.Ingested && !showAll {
					continue
				}
				tag := ""
				if s.Ingested {
					tag = " [ingested]"
				}
				proj := ""
				if s.Project != "" {
					proj = fmt.Sprintf(" (%s)", s.Project)
				}
				fmt.Printf("%-16s %-20s %s%s%s\n",
					s.Source, s.ID[:min(20, len(s.ID))],
					s.Timestamp.Format("2006-01-02 15:04"), proj, tag)
				count++
			}
			if count == 0 {
				fmt.Println("No new sessions found.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "include already-ingested sessions")
	return cmd
}

func ingestCmd() *cobra.Command {
	var claudeCode string
	var cursorTranscript string
	var workspace string
	var project string

	cmd := &cobra.Command{
		Use:   "ingest [conversation-id]",
		Short: "Ingest an AI session into the knowledge pipeline",
		Long: `Parse a raw AI coding session and create a trace + learning .md file.

Three flows:
  1. Cursor raw/:           ctx ingest <conversation-id>
  2. Claude Code JSONL:     ctx ingest --claude-code <path>
  3. Cursor transcript:     ctx ingest --cursor-transcript <path>`,
		Example: `  ctx ingest abc123def
  ctx ingest --claude-code ~/.claude/projects/my-proj/sessions/sess.jsonl
  ctx ingest --cursor-transcript path/to/transcript.txt --project my-proj`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var result *pipeline.IngestResult

			switch {
			case claudeCode != "":
				result, err = pipeline.IngestClaudeCode(dataStore, cfg, claudeCode)
			case cursorTranscript != "":
				result, err = pipeline.IngestCursorTranscript(dataStore, cfg, cursorTranscript, workspace, project)
			case len(args) == 1:
				result, err = pipeline.IngestCursorRaw(dataStore, cfg, args[0])
			default:
				return fmt.Errorf("provide a conversation-id, --claude-code <path>, or --cursor-transcript <path>")
			}

			if err != nil {
				return err
			}
			fmt.Printf("Ingested %s → %s\n", result.TraceID, result.LearnMD)
			return nil
		},
	}
	cmd.Flags().StringVar(&claudeCode, "claude-code", "", "path to Claude Code JSONL session file")
	cmd.Flags().StringVar(&cursorTranscript, "cursor-transcript", "", "path to Cursor transcript .txt file")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace path hint (for cursor-transcript)")
	cmd.Flags().StringVar(&project, "project", "", "project name hint (for cursor-transcript)")
	return cmd
}

func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hook",
		Short: "Handle a Cursor hook event (stdin)",
		Long: `Receive a Cursor hook JSON event on stdin and save it to ~/.context/raw/.

Configure in Cursor as the hook handler:
  "hookCommand": "ctx hook"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return capture.HandleHook(cfg.ContextDir(), os.Stdin)
		},
	}
}

func summarizeCmd() *cobra.Command {
	var all bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "summarize [trace-id]",
		Short: "AI-summarize traces into structured knowledge",
		Long: `Extract portable patterns and domain knowledge from traces using LLM.

Produces: problem, pattern, skills[], pitfalls, embedding_text (portable)
          plus project_tag, component, domain_knowledge (domain-specific).

By default, processes only unsummarized traces. Use --all to reprocess.`,
		Example: `  ctx summarize                  # summarize new traces
  ctx summarize --all            # reprocess all traces
  ctx summarize --dry-run        # show what would be processed
  ctx summarize trace-abc12345   # summarize a specific trace`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			client, err := llm.New(cfg, dataStore)
			if err != nil {
				return fmt.Errorf("init llm: %w", err)
			}

			ctx := context.Background()

			// Specific trace
			if len(args) == 1 {
				trace, err := dataStore.GetTrace(args[0])
				if err != nil {
					return err
				}
				if trace == nil {
					return fmt.Errorf("trace %s not found", args[0])
				}
				sm, err := pipeline.SummarizeTrace(ctx, dataStore, client, cfg, trace)
				if err != nil {
					return err
				}
				fmt.Printf("Summarized %s → skills: %s [%s]\n", trace.ID, sm.Skills, sm.ProjectTag)
				return nil
			}

			// Dry run
			if dryRun {
				var traces []store.Trace
				if all {
					traces, err = dataStore.ListTraces()
				} else {
					traces, err = dataStore.UnsummarizedTraces()
				}
				if err != nil {
					return err
				}
				if len(traces) == 0 {
					fmt.Println("All traces already summarized (use --all to reprocess)")
					return nil
				}
				fmt.Printf("%d trace(s) to process:\n", len(traces))
				for _, t := range traces {
					p := t.Prompt
					if len(p) > 60 {
						p = p[:60]
					}
					if p == "" {
						p = "(no prompt)"
					}
					fmt.Printf("  %s  %s  %s  %s\n", t.ID, t.Source, t.Project, p)
				}
				return nil
			}

			// Batch
			result, err := pipeline.SummarizeAll(ctx, dataStore, client, cfg, all)
			if err != nil {
				return err
			}
			if result.Total == 0 {
				fmt.Println("All traces already summarized (use --all to reprocess)")
				return nil
			}
			fmt.Printf("Summarized %d/%d traces (%d failed)\n", result.Succeeded, result.Total, result.Failed)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "reprocess all traces")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be processed")
	return cmd
}

func embedCmd() *cobra.Command {
	var all bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "embed [trace-id]",
		Short: "Embed summaries and namespace files into vectors",
		Long: `Create vector embeddings for trace summaries and namespace .md files.

Trace summaries → embeddings table (384-dim, all-MiniLM-L6-v2).
Namespace files → chunks table (split on ## headers, then embedded).

By default, only embeds new/unembedded items. Use --all to re-embed.`,
		Example: `  ctx embed                      # embed new items
  ctx embed --all                # re-embed everything
  ctx embed --dry-run            # show what would be embedded
  ctx embed trace-abc12345       # embed a specific trace`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			embedder := vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			ctx := context.Background()

			// Specific trace
			if len(args) == 1 {
				if err := pipeline.IndexTrace(ctx, dataStore, embedder, args[0]); err != nil {
					return err
				}
				fmt.Printf("Embedded %s\n", args[0])
				return nil
			}

			// Dry run
			if dryRun {
				var summaries []store.Summary
				if all {
					summaries, err = dataStore.AllSummaries()
				} else {
					summaries, err = dataStore.UnembeddedSummaries()
				}
				if err != nil {
					return err
				}
				fmt.Printf("%d trace(s) to embed\n", len(summaries))
				for _, sm := range summaries {
					text := sm.EmbeddingText
					if len(text) > 60 {
						text = text[:60]
					}
					fmt.Printf("  [trace] %s: %s...\n", sm.TraceID, text)
				}
				return nil
			}

			// Full index
			result, err := pipeline.IndexAll(ctx, dataStore, embedder, cfg, all)
			if err != nil {
				return err
			}
			if result.TracesTotal == 0 && result.FilesTotal == 0 {
				fmt.Println("Nothing to embed (use --all to re-embed)")
				return nil
			}
			fmt.Printf("Embedded %d/%d traces, %d/%d files (%d chunks)",
				result.TracesEmbedded, result.TracesTotal,
				result.FilesEmbedded, result.FilesTotal,
				result.ChunksCreated)
			if result.StalesPruned > 0 {
				fmt.Printf(", pruned %d stale", result.StalesPruned)
			}
			fmt.Println()
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "re-embed everything")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be embedded")
	return cmd
}

func detectCmd() *cobra.Command {
	var force bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Detect recurring patterns across traces",
		Long: `Analyze summarized traces to find recurring skills, co-occurring slug pairs,
and repeated pitfalls. Produces candidate definitions via LLM.

By default, only runs after 20+ new traces since last detection.
Use --force to bypass this gate.`,
		Example: `  ctx detect                     # run detection
  ctx detect --dry-run           # show what would be detected
  ctx detect --force             # bypass the trace counter gate`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			client, err := llm.New(cfg, dataStore)
			if err != nil {
				return fmt.Errorf("init llm: %w", err)
			}

			ctx := context.Background()
			result, err := pipeline.Detect(ctx, dataStore, client, cfg, force, dryRun)
			if err != nil {
				return err
			}

			if result.Candidates == 0 && result.Skipped == 0 {
				fmt.Println("No candidates found above threshold")
				return nil
			}
			if dryRun {
				fmt.Printf("%d candidate(s) to synthesize\n", result.Candidates)
			} else {
				fmt.Printf("Created %d candidate(s), skipped %d\n", result.Candidates, result.Skipped)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "bypass the trace counter gate")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be detected")
	return cmd
}

func compileCmd() *cobra.Command {
	var tag string

	cmd := &cobra.Command{
		Use:   "compile <task>",
		Short: "Build a context briefing for a task",
		Long: `Search all knowledge (trace embeddings + namespace chunks) for items
relevant to the given task. Produces a grounded briefing with:
  - Unified approach paragraph (3-5 sentences)
  - Relevant patterns, skills, and learnings
  - Open questions

Output: ~/.context/compiled/<slug>.md + .json`,
		Example: `  ctx compile "design a decomposable judge metric"
  ctx compile --tag coach-chatbot "extend evals"
  ctx compile "refactor the CLI"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			client, err := llm.New(cfg, dataStore)
			if err != nil {
				return fmt.Errorf("init llm: %w", err)
			}
			embedder := vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			ctx := context.Background()

			result, err := pipeline.Compile(ctx, dataStore, client, embedder, cfg, args[0], tag)
			if err != nil {
				return err
			}

			fmt.Printf("Compiled: %s\n", result.MDPath)
			fmt.Printf("Sources: %d traces + %d chunks\n", result.TraceHits, result.ChunkHits)
			if result.Approach != "" {
				fmt.Printf("\nApproach:\n%s\n", result.Approach)
			}
			if len(result.Questions) > 0 {
				fmt.Println("\nOpen questions:")
				for _, q := range result.Questions {
					fmt.Printf("  - %s\n", q)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "filter by project tag")
	return cmd
}

func synthesizeCmd() *cobra.Command {
	var sources []string
	var output string
	var synthType string
	var ctxStr string
	var refine string
	var feedback string

	cmd := &cobra.Command{
		Use:   "synthesize",
		Short: "Create or refine skills from source material",
		Long: `Synthesize reusable skills or patterns from URLs, files, or existing knowledge.
Chunks, embeds, and registers the result in the knowledge table.

Supports refinement of existing skills with --refine + --feedback.`,
		Example: `  ctx synthesize --source https://example.com/article --output skills/external/eval-design
  ctx synthesize --source doc.md --source ref.md --output patterns/context-injection --type pattern
  ctx synthesize --refine skills/internal/eval-design --source new-paper.md --feedback "add calibration"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			client, err := llm.New(cfg, dataStore)
			if err != nil {
				return fmt.Errorf("init llm: %w", err)
			}
			embedder := vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			ctx := context.Background()

			result, err := pipeline.Synthesize(ctx, dataStore, client, embedder, cfg, pipeline.SynthesizeOpts{
				Sources:  sources,
				Output:   output,
				Type:     synthType,
				Context:  ctxStr,
				Refine:   refine,
				Feedback: feedback,
			})
			if err != nil {
				return err
			}

			fmt.Printf("Synthesized: %s (%d chunks)\n", result.FilePath, result.Chunks)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&sources, "source", nil, "source material (URL or file path, repeatable)")
	cmd.Flags().StringVar(&output, "output", "", "output path relative to ~/.context/")
	cmd.Flags().StringVar(&synthType, "type", "skill", "synthesis type: skill or pattern")
	cmd.Flags().StringVar(&ctxStr, "context", "", "calibration context about your work")
	cmd.Flags().StringVar(&refine, "refine", "", "path to existing skill to refine")
	cmd.Flags().StringVar(&feedback, "feedback", "", "what to improve (used with --refine)")
	return cmd
}

func pushCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "push <file|url>",
		Short: "Add a file or URL to the knowledge base",
		Long: `Push a local file or URL into a namespace directory. The content is copied,
chunked, embedded, and registered in the knowledge table.`,
		Example: `  ctx push design.md -n docs
  ctx push https://example.com/guide -n references
  ctx push patterns/error-handling.md -n patterns`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if namespace == "" {
				return fmt.Errorf("--namespace (-n) required")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			embedder := vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			ctx := context.Background()

			if err := pipeline.Push(ctx, dataStore, embedder, cfg, args[0], namespace); err != nil {
				return err
			}
			fmt.Printf("Pushed to %s/\n", namespace)
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "target namespace (docs, patterns, etc.)")
	return cmd
}

func noteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "note <text>",
		Short: "Create a quick note",
		Long:  "Write a timestamped note to ~/.context/learnings/.",
		Example: `  ctx note "TIL: Go's embed package shadows internal/embed"
  ctx note "Remember to add error wrapping in the CLI layer"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			path, err := pipeline.Note(dataStore, cfg, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Note saved: %s\n", path)
			return nil
		},
	}
}

func tagCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "tag <file> <tag>",
		Short:   "Tag a knowledge item",
		Example: `  ctx tag patterns/error-handling.md important`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return pipeline.Tag(dataStore, cfg, args[0], args[1])
		},
	}
}

func forgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "forget <id>",
		Short:   "Remove a knowledge item by ID",
		Example: `  ctx forget ns-patterns-error-handling`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := pipeline.Forget(dataStore, args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", args[0])
			return nil
		},
	}
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file and its DB entries",
		Long:  "Remove a file from ~/.context/ and clean up associated knowledge and chunk entries.",
		Example: `  ctx rm docs/old-guide.md
  ctx rm patterns/obsolete.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := pipeline.Remove(dataStore, cfg, args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", args[0])
			return nil
		},
	}
}

func backfillCmd() *cobra.Command {
	var dryRun bool
	var limit int
	var after string

	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Discover + ingest + summarize + embed in one step",
		Long: `Run the full pipeline: discover new sessions, ingest them, summarize
with LLM, and embed vectors. Convenient for catching up.`,
		Example: `  ctx backfill                   # process everything new
  ctx backfill --dry-run         # show what would be processed
  ctx backfill --limit 10        # process at most 10 sessions
  ctx backfill --after 2024-01-01`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var client *llm.Client
			if !dryRun {
				client, err = llm.New(cfg, dataStore)
				if err != nil {
					return fmt.Errorf("init llm: %w", err)
				}
			}
			var embedder *vectors.Embedder
			if !dryRun {
				embedder = vectors.New(cfg.API.OllamaBaseURL, "all-minilm")
			}

			ctx := context.Background()
			result, err := pipeline.Backfill(ctx, dataStore, client, embedder, cfg, pipeline.BackfillOpts{
				DryRun: dryRun,
				Limit:  limit,
				After:  after,
			})
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Printf("Discovered %d sessions, %d to ingest\n", result.Discovered, result.Ingested)
			} else {
				fmt.Printf("Backfill: %d discovered, %d ingested, %d summarized, %d embedded\n",
					result.Discovered, result.Ingested, result.Summarized, result.Embedded)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be processed")
	cmd.Flags().IntVar(&limit, "limit", 0, "max sessions to ingest (0 = unlimited)")
	cmd.Flags().StringVar(&after, "after", "", "only sessions after this date (YYYY-MM-DD)")
	return cmd
}

func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Auto-ingest new Claude Code sessions",
		Long: `Monitor the Claude Code sessions directory for new JSONL files
and automatically ingest them as they appear.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := context.Background()
			return capture.Watch(ctx, dataStore, cfg)
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show pipeline health and stats",
		Long:  "Display row counts for all tables and key metadata timestamps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			stats, err := dataStore.Stats()
			if err != nil {
				return err
			}

			fmt.Println("ctx — pipeline status")
			fmt.Println()
			fmt.Println("Tables:")
			for _, name := range []string{"entries", "traces", "summaries", "embeddings", "candidates", "knowledge", "chunks", "usage"} {
				fmt.Printf("  %-14s %d\n", name, stats[name])
			}

			fmt.Println()
			fmt.Println("Timestamps:")
			for _, key := range []string{"last_summarize_ts", "last_embed_ts", "last_detection_run"} {
				val, _ := dataStore.GetMeta(key)
				if val == "" {
					val = "(never)"
				}
				fmt.Printf("  %-24s %s\n", key, val)
			}

			// Count unsummarized and unembedded
			unsumm, _ := dataStore.UnsummarizedTraces()
			unemb, _ := dataStore.UnembeddedSummaries()
			fmt.Println()
			fmt.Println("Pending:")
			fmt.Printf("  %-24s %d\n", "unsummarized traces", len(unsumm))
			fmt.Printf("  %-24s %d\n", "unembedded summaries", len(unemb))

			return nil
		},
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the web UI",
		Long: `Start the ctx web dashboard on localhost.

The 7-tab UI provides:
  - Compile: build context briefings from your knowledge
  - Learnings: browse all ingested traces and summaries
  - Synthesize: create skills from external sources
  - Candidates: approve/dismiss detected patterns
  - Namespace: browse and manage knowledge files
  - Graph: visualize skill co-occurrence
  - Usage: track LLM API costs`,
		Example: `  ctx serve              # start on default port (7337)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return web.Serve(dataStore, cfg)
		},
	}
}
