package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/benchnav"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/tokens"
)

const benchNavUsage = `Usage: dex bench nav <project-path> [flags]

Navigation benchmark (epic #316, story 7) — the RULER for the explore stories.
Measures how much work an agent spends to first TOUCH a gold file, under a
deterministic, zero-inference policy: issue one find(query), then read the
ranked files top-down until a relevant file is opened. Reports reach-rate and
the calls + tokens to that first touch.

This is the NO-MAP baseline. Once 'dex map' lands (story 1) the same lane will
re-measure with a map() seeding navigation and report the delta — the whole
point of the explore epic is to drive these numbers down.

Reuses the committed eval golden set (query -> relevant files), so the query
set is stable across runs. Like 'dex bench eval', this needs a live index and
embedder; it is a local-compute instrument, not part of the CI gate.

Flags:
  --golden path   golden-set JSON (default: <project>/benchmark/eval/golden.json)
  --k int         read horizon — how deep the agent reads before giving up (default: 10)
  --lane name     retrieval lane: full (default) | bm25 | onnx
  --output format json or md (default: md)
  --check path    compare against a reference report JSON; exit 1 on regression

Env: DEX_EMBED_URL, DEX_EMBED_MODEL, DEX_EMBED_BATCH — same as indexing.
`

func runBenchNav(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("bench nav", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchNavUsage) }

	goldenPath := fs.String("golden", "", "golden-set JSON path")
	k := fs.Int("k", 10, "read horizon")
	lane := fs.String("lane", "full", "retrieval lane: full | bm25 | onnx")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference report JSON to check for regression")

	// Project path is the first non-flag arg; allow flags after it.
	var projectPath string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || projectPath != "" {
			flagArgs = append(flagArgs, a)
		} else {
			projectPath = a
		}
	}
	if projectPath == "" {
		fs.Usage()
		os.Exit(1)
	}
	_ = fs.Parse(flagArgs)

	p, err := resolveEvalProject(projectPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: %v\n", err)
		os.Exit(1)
	}

	gPath := *goldenPath
	if gPath == "" {
		gPath = filepath.Join(p.Root, "benchmark", "eval", "golden.json")
	}
	gs, err := eval.LoadGolden(gPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: load golden set: %v\n (generate one with: dex bench eval %s --gen)\n", err, projectPath)
		os.Exit(1)
	}
	if len(gs.Queries) == 0 {
		fmt.Fprintln(os.Stderr, "dex bench nav: golden set is empty")
		os.Exit(1)
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: no index for %s — run `dex index %s` first\n", p.Root, p.Root)
		os.Exit(1)
	}
	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: open store: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	stats, _ := st.Stats(ctx)
	em := evalEmbedForLane(*lane, stats.EmbedModel)

	fmt.Fprintf(os.Stderr, "dex bench nav: %d queries, k=%d, lane=%s, index %s\n", len(gs.Queries), *k, *lane, p.DBPath)

	results, err := eval.RunWithRewrite(ctx, em, st, gs, *k, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: run: %v\n", err)
		os.Exit(1)
	}

	queries := make([]benchnav.Query, 0, len(results))
	for _, r := range results {
		queries = append(queries, benchnav.Query{
			Query:    r.Query,
			Ranked:   r.RankedFiles,
			Relevant: r.Relevant,
		})
	}

	rep := benchnav.Compute(queries, *k, navCostModel(p.Root), *lane)

	switch *outputFmt {
	case "json":
		out, err := rep.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench nav: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(rep.Markdown())
	}

	if *checkPath != "" {
		ref, err := loadNavReport(*checkPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench nav: load --check report: %v\n", err)
			os.Exit(1)
		}
		// reach-rate may not fall >2 pts; mean calls/tokens may not rise >5%.
		regs := rep.Regressions(ref, 0.02, 0.05)
		if len(regs) > 0 {
			fmt.Fprintln(os.Stderr, "dex bench nav: regression check FAILED:")
			for _, r := range regs {
				fmt.Fprintf(os.Stderr, "  - %s\n", r)
			}
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "dex bench nav: regression check passed")
	}
}

// navCostModel prices the agent's actions for the no-map policy. A read costs
// the token count of the file's content at the project root (memoized — gold
// files recur across queries); the find envelope is modeled as the token count
// of the ranked path list the agent scans before opening anything.
func navCostModel(root string) benchnav.CostModel {
	cache := make(map[string]int)
	return benchnav.CostModel{
		Read: func(path string) int {
			if n, ok := cache[path]; ok {
				return n
			}
			n := 0
			if b, err := os.ReadFile(filepath.Join(root, path)); err == nil {
				n = tokens.Count(string(b))
			}
			cache[path] = n
			return n
		},
		FindEnvelope: func(ranked []string) int {
			return tokens.Count(strings.Join(ranked, "\n"))
		},
	}
}

func loadNavReport(path string) (benchnav.Report, error) {
	var rep benchnav.Report
	b, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(b, &rep)
}
