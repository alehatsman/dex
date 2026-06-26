package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
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
	intent := fs.String("intent", "", "force a strategy: auto|behavior_search|symbol_lookup|callers|callees|architecture|package_topology|editing_context|assemble")
	k := fs.Int("k", 8, "max hits per lane (capped at 30)")
	format := fs.String("format", "text", "output format: text | json")
	noInline := fs.Bool("no-inline", false, "skip inlining raw file contents into suggested_reads (stored chunk/file summaries are still emitted; use --format=json to inspect)")
	maxContentBytes := fs.Int("max-content-bytes", 0, "truncate content display at N bytes (0 = no limit; applies to text output only)")
	expand := fs.String("expand", "", "query-side expansion (#252): off|on|full (empty defers to DEX_EXPAND_MODE). Requires DEX_EXPAND_MODEL; otherwise a no-op.")
	verbose := fs.Bool("v", false, "verbose: show wall-clock timing")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if !validIntent(*intent) {
		return fmt.Errorf("invalid --intent=%q (want one of: auto, behavior_search, symbol_lookup, callers, callees, architecture, package_topology, editing_context, assemble)", *intent)
	}
	if !validExpandMode(*expand) {
		return fmt.Errorf("invalid --expand=%q (want off|on|full, or empty to use DEX_EXPAND_MODE)", *expand)
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
	in := mcp.ContextInput{
		ProjectRoot: p.Root,
		Question:    question,
		Intent:      *intent,
		K:           *k,
		NoInline:    *noInline,
		Expand:      *expand,
	}

	// Stream the synthesized answer to stdout token-by-token when writing
	// to an interactive terminal in text mode: the first token lands in
	// ~300ms instead of blocking ~1.5–3s for the whole answer. Piped and
	// JSON output stay one-shot so byte output is stable for consumers; a
	// cache hit skips the sink and the answer is printed below as usual.
	streaming := (*format == "" || *format == "text") && stdoutIsTTY() && s.ChatClient != nil
	var streamed strings.Builder
	var sink func(string)
	if streaming {
		model := s.ChatClient.ModelName()
		first := true
		sink = func(tok string) {
			if first {
				if model != "" {
					fmt.Printf("answer (%s):\n", model)
				} else {
					fmt.Print("answer:\n")
				}
				first = false
			}
			fmt.Print(tok)
			streamed.WriteString(tok)
		}
	}

	t0 := time.Now()
	_, out, err := s.ContextRouterStream(ctx, in, sink)
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
		// When tokens were streamed, the answer is already on stdout.
		// Emit any suffix the synthesis layer appended after streaming
		// (the citation-guard note), then render the rest of the bundle
		// with the answer block suppressed to avoid a double print.
		answerHandled := false
		if streamed.Len() > 0 {
			if suffix := strings.TrimPrefix(out.Answer, strings.TrimSpace(streamed.String())); suffix != "" && suffix != out.Answer {
				fmt.Print(suffix)
			}
			fmt.Print("\n\n")
			answerHandled = true
		}
		printContextText(out, *maxContentBytes, answerHandled)
		if *verbose {
			fmt.Fprintf(os.Stderr, "timing: %dms\n", elapsed.Milliseconds())
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want text|json)", *format)
	}
}

// ─── index status ──────────────────────────────────────────────────────────
