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

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func cmdSearch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("search needs a subcommand: semantic | symbol")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "semantic":
		return cmdSearchSemantic(ctx, rest)
	case "symbol":
		return cmdSearchSymbol(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex search semantic <path> <query...>   hybrid top-k chunks (MCP: find)
  dex search symbol   <path> <name>       exact identifier lookup (MCP: lookup)`)
		return nil
	default:
		return fmt.Errorf("unknown search subcommand: %s (have: semantic, symbol)", sub)
	}
}

func cmdSearchSemantic(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search semantic", flag.ContinueOnError)
	setHelp(fs,
		"Hybrid top-k chunks for a query (MCP: find).",
		"dex search semantic [flags] [<path>] <query...>",
		`dex search semantic . "retry logic"`,
		`dex search semantic . --k=16 --explain "rate limiter"`,
		`dex search semantic . --max-content-bytes=4000 "error handling"`,
	)
	k := fs.Int("k", 8, "number of results to return")
	rerankFlag := fs.String("rerank", "", "set to 'off' to skip the rerank stage for this query (no effect when DEX_RERANK_URL is unset)")
	format := fs.String("format", "text", "output format: text | json")
	explain := fs.Bool("explain", false, "show per-chunk score breakdown and stage timings")
	maxContentBytes := fs.Int("max-content-bytes", 1500, "truncate content display at N bytes (0 = no limit)")
	verbose := fs.Bool("v", false, "verbose: show score breakdown and timing (equivalent to --explain)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if *verbose {
		*explain = true
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("search semantic needs a query (path defaults to cwd)")
	}
	q := strings.Join(rest, " ")
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("query is empty — pass a natural-language description or code fragment")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, p.Root)
		}
		return err
	}
	opts := storeOpts()
	if *rerankFlag == "off" {
		opts.Reranker = nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, opts)
	if err != nil {
		return err
	}
	defer st.Close()
	em := newEmbedClient(st.EmbedModel())
	var queryVec []float32
	var embedDur time.Duration
	if em != nil {
		t0 := time.Now()
		vecs, err := em.Embed(ctx, []string{q})
		embedDur = time.Since(t0)
		if err != nil {
			if !errors.Is(err, embed.ErrUnreachable) {
				return err
			}
			// Degrade, don't crash: drop the semantic leg and run BM25-only.
			fmt.Fprintf(os.Stderr,
				"dex: embedding service offline at %s — degraded to BM25-only (start ollama, or set DEX_EMBED_URL)\n",
				em.Endpoint())
		} else {
			queryVec = vecs[0]
		}
	}
	t1 := time.Now()
	hits, err := st.Search(ctx, queryVec, q, *k)
	searchDur := time.Since(t1)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return nil
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hitsToJSON(hits))
	case "", "text":
		for i, h := range hits {
			loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
			if h.Name != "" {
				loc = h.Name + "  " + loc
			}
			header := fmt.Sprintf("─── #%d %s  (%s)", i+1, loc, h.Kind)
			if *explain {
				scores := fmt.Sprintf("sem=%.4f", h.Score)
				if h.BM25Score > 0 {
					scores += fmt.Sprintf("  bm25=%.4f", h.BM25Score)
				}
				if h.RRFScore > 0 {
					scores += fmt.Sprintf("  rrf=%.4f", h.RRFScore)
				}
				if h.RerankScore > 0 {
					scores += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
				}
				fmt.Println(header)
				fmt.Println("  " + scores)
			} else {
				header += fmt.Sprintf("  score=%.4f", h.Score)
				if h.RerankScore > 0 {
					header += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
				}
				fmt.Println(header)
			}
			fmt.Println(truncate(h.Content, *maxContentBytes))
			fmt.Println()
		}
		if *explain {
			fmt.Fprintf(os.Stderr, "timing:  embed=%dms  search=%dms  total=%dms\n",
				embedDur.Milliseconds(), searchDur.Milliseconds(), (embedDur + searchDur).Milliseconds())
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want text|json)", *format)
	}
}

// cmdSearchSymbol wraps the MCP `search_symbol` tool. Exact identifier
// lookup against the indexed chunks — no embedding required.
func cmdSearchSymbol(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search symbol", flag.ContinueOnError)
	setHelp(fs,
		"Exact identifier lookup (MCP: lookup).",
		"dex search symbol [flags] [<path>] <name>",
		`dex search symbol . "RateLimiter"`,
		`dex search symbol . --k=20 "func.*Handler"`,
	)
	k := fs.Int("k", 10, "max results to return")
	format := fs.String("format", "text", "output format: text | json")
	maxContentBytes := fs.Int("max-content-bytes", 1500, "truncate content display at N bytes (0 = no limit)")
	_ = fs.Bool("v", false, "verbose (accepted, currently no-op)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("search symbol needs a name (path defaults to cwd) — e.g. `dex search symbol Watcher`")
		}
		return fmt.Errorf("search symbol takes one <name> (got %d extra args)", len(rest)-1)
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
	out, err := s.FindSymbol(ctx, mcp.FindSymbolInput{
		Name:        rest[0],
		ProjectRoot: p.Root,
		K:           *k,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printSearchHitResult(out.Status, out.Hint, out.Project, out.Hits, *maxContentBytes)
	return nil
}

// ─── ask ──────────────────────────────────────────────────────────────────

// cmdAsk mirrors the `ask` MCP tool so agents and humans share one
// implementation. The flag surface maps 1-to-1 onto mcp.ContextInput
// so a CLI invocation can stand in for a tool call when an agent is
// offline.
