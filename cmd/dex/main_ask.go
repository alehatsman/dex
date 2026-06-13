package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func cmdAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	setHelp(fs,
		"One-shot router — composes semantic + symbol + graph; emit suggested_reads + next_action. Use this BEFORE grep loops.",
		"dex ask [flags] [<path>] <question...>",
		`dex ask . "where is the rate limiter?"`,
		`dex ask . --intent symbol_lookup "RateLimiter"`,
		`dex ask . --format json "retry logic" | jq .suggested_reads`,
	)
	intent := fs.String("intent", "", "force a strategy: auto|behavior_search|symbol_lookup|callers|callees|architecture|package_topology|editing_context")
	k := fs.Int("k", 8, "max hits per lane (capped at 30)")
	format := fs.String("format", "text", "output format: text | json")
	noInline := fs.Bool("no-inline", false, "skip inlining raw file contents into suggested_reads (stored chunk/file summaries are still emitted; use --format=json to inspect)")
	maxContentBytes := fs.Int("max-content-bytes", 0, "truncate content display at N bytes (0 = no limit; applies to text output only)")
	verbose := fs.Bool("v", false, "verbose: show wall-clock timing")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if !validIntent(*intent) {
		return fmt.Errorf("invalid --intent=%q (want one of: auto, behavior_search, symbol_lookup, callers, callees, architecture, package_topology, editing_context)", *intent)
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("ask needs a question (path defaults to cwd) — e.g. `dex ask \"where is the watcher?\"`")
	}
	question := strings.Join(rest, " ")
	if strings.TrimSpace(question) == "" {
		return fmt.Errorf("question is empty — pass a natural-language description")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	s, _ := newServerFromEnv(base)
	t0 := time.Now()
	_, out, err := s.ContextRouter(ctx, mcp.ContextInput{
		ProjectRoot: p.Root,
		Question:    question,
		Intent:      *intent,
		K:           *k,
		NoInline:    *noInline,
	})
	elapsed := time.Since(t0)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "", "text":
		printContextText(out, *maxContentBytes)
		if *verbose {
			fmt.Fprintf(os.Stderr, "timing: %dms\n", elapsed.Milliseconds())
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want text|json)", *format)
	}
}

// ─── view ──────────────────────────────────────────────────────────────────

// cmdView dispatches `dex view <summarize>`. Mirrors the MCP
// `file_view` tool by parking it under a `view` group so future
// view-style ops (e.g. `view chunk`) land next to it.
func cmdView(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("view needs a subcommand: summarize")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "summarize":
		return cmdViewSummarize(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex view summarize <path> <file>   summarize a file slice (MCP: read)`)
		return nil
	default:
		return fmt.Errorf("unknown view subcommand: %s (have: summarize)", sub)
	}
}

func cmdViewSummarize(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("view summarize", flag.ContinueOnError)
	setHelp(fs,
		"Summarize a file slice via the chat model (MCP: read).",
		"dex view summarize [flags] [<path>] <file>")
	start := fs.Int("start", 0, "first line to summarize (1-indexed; 0 = beginning of file)")
	end := fs.Int("end", 0, "last line to summarize (1-indexed, inclusive; 0 = end of file)")
	focus := fs.String("focus", "", "optional steering — e.g. 'public API surface', 'side effects'")
	temp := fs.Float64("temperature", 0, "sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "max tokens to generate (0 = server default)")
	format := fs.String("format", "text", "output format: text | json")
	verbose := fs.Bool("v", false, "include model name and other debug headers in text output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("view summarize needs a <file> (path defaults to cwd)")
		}
		return fmt.Errorf("view summarize takes one <file> (got %d extra args)", len(rest)-1)
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.Summarize(ctx, mcp.SummarizeInput{
		Path:        rest[0],
		ProjectRoot: p.Root,
		StartLine:   *start,
		EndLine:     *end,
		Focus:       *focus,
		Temperature: float32(*temp),
		MaxTokens:   *maxTok,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	fmt.Printf("file:  %s (lines %d-%d, %d bytes", out.Path, out.StartLine, out.EndLine, out.Bytes)
	if out.Truncated {
		fmt.Print(", truncated")
	}
	fmt.Println(")")
	if *verbose && out.Model != "" {
		fmt.Printf("model: %s\n", out.Model)
	}
	fmt.Println()
	fmt.Println(out.Content)
	if out.FinishReason != "" && out.FinishReason != "stop" {
		fmt.Fprintf(os.Stderr, "\n(finish_reason=%s)\n", out.FinishReason)
	}
	return nil
}

// ─── generate ──────────────────────────────────────────────────────────────

func cmdGenerate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	setHelp(fs,
		"Generate code grounded in the project's index (RAG: top-k chunks → chat endpoint).",
		"dex generate [flags] [<path>] <prompt...>")
	k := fs.Int("k", 8, "number of RAG chunks to retrieve as context")
	noRAG := fs.Bool("no-rag", false, "skip retrieval; send prompt to the chat endpoint without project context")
	system := fs.String("system", "", "override the default system prompt")
	temp := fs.Float64("temperature", 0, "sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "max tokens to generate (0 = server default)")
	showCtx := fs.Bool("show-context", false, "print the chunks fed as context before the model output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("generate needs a prompt (path defaults to cwd)")
	}
	prompt := strings.Join(rest, " ")
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is empty")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	var hits []store.Hit
	if !*noRAG {
		if _, err := os.Stat(p.DBPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("no index for %s — run `dex index %s` first, or pass --no-rag to skip retrieval", p.Root, p.Root)
			}
			return err
		}
		st, err := openStore(ctx, p.DBPath)
		if err != nil {
			return err
		}
		em := newEmbedClient(st.EmbedModel())
		if em == nil {
			st.Close()
			return fmt.Errorf("RAG requires an embedding model; re-run with --no-rag or configure DEX_EMBED_URL")
		}
		vecs, err := em.Embed(ctx, []string{prompt})
		if err != nil {
			st.Close()
			return fmt.Errorf("embed: %w", err)
		}
		hits, err = st.Search(ctx, vecs[0], prompt, *k)
		st.Close()
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
	}

	sysPrompt := *system
	if strings.TrimSpace(sysPrompt) == "" {
		sysPrompt = "You are a precise coding assistant. " +
			"When CONTEXT chunks from the user's project are provided, ground your answer in them — " +
			"reference real symbols, paths, and conventions rather than inventing names. " +
			"Output code in fenced blocks; keep prose minimal."
	}

	userContent := prompt
	if len(hits) > 0 {
		userContent = store.FormatHits(hits) + "\n\n---\n\n" + prompt
	}

	if *showCtx && len(hits) > 0 {
		fmt.Fprintln(os.Stderr, "─── context fed to the model ───")
		for i, h := range hits {
			fmt.Fprintf(os.Stderr, "#%d %s:%d-%d  (%s)  score=%.4f\n", i+1, h.Path, h.StartLine, h.EndLine, h.Kind, h.Score)
		}
		fmt.Fprintln(os.Stderr, "────────────────────────────────")
	}

	cc := newChatClient()
	resp, err := cc.Generate(ctx, []chat.Message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userContent},
	}, chat.Options{
		Temperature: float32(*temp),
		MaxTokens:   *maxTok,
	})
	if err != nil {
		return err
	}
	fmt.Println(resp.Content)
	if resp.FinishReason != "" && resp.FinishReason != "stop" {
		fmt.Fprintf(os.Stderr, "\n(finish_reason=%s)\n", resp.FinishReason)
	}
	return nil
}

// ─── index status ──────────────────────────────────────────────────────────
