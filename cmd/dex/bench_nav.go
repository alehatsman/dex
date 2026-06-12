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
	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/tokens"
)

const benchNavUsage = `Usage: dex bench nav <project-path> [flags]

Navigation benchmark (epic #316, story 7) — the RULER for the explore stories.
Measures how much work an agent spends to first TOUCH a gold file, under a
deterministic, zero-inference policy: issue one find(query), then read the
ranked files top-down until a relevant file is opened. Reports reach-rate and
the calls + tokens to that first touch.

It reports BOTH lanes: the no-map baseline, and a map-seeded lane that issues one
'dex map' L0 to orient, zooms the cluster naming the gold file (L1), then opens it
— plus the delta between them. Driving that delta negative is the whole point of
the explore epic.

Reuses the committed eval golden set (query -> relevant files), so the query
set is stable across runs. Like 'dex bench eval', this needs a live index and
embedder; it is a local-compute instrument, not part of the CI gate.

Flags:
  --golden path   golden-set JSON (default: <project>/benchmark/eval/golden.json)
  --k int         read horizon — how deep the agent reads before giving up (default: 10)
  --lane name     retrieval lane: full (default) | bm25 | onnx
  --l0-budget int map-lane L0 token budget (default: 150)
  --l1-budget int map-lane L1 token budget per cluster (default: 1000)
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
	l0budget := fs.Int("l0-budget", 0, "map-lane L0 token budget (default 150)")
	l1budget := fs.Int("l1-budget", 0, "map-lane L1 token budget per cluster (default 1000)")
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

	cost := navCostModel(p.Root)
	noMap := benchnav.Compute(queries, *k, cost, *lane)

	// Phase B: seed navigation with `dex map`. Build the same L0/L1 the agent
	// would see by default, then measure the map-vs-no-map delta. If the graph has
	// no communities (e.g. BM25-only index), report the no-map lane alone.
	mapModel, mapErr := buildNavMapModel(ctx, p.Root, *l0budget, *l1budget)
	if mapErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: map lane unavailable (%v) — reporting no-map only\n", mapErr)
		emitNavReport(noMap, *outputFmt)
		return
	}
	mapSeeded := benchnav.ComputeMap(queries, cost, mapModel, *lane)
	cmp := benchnav.Compare(noMap, mapSeeded)

	switch *outputFmt {
	case "json":
		out, err := cmp.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench nav: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(cmp.Markdown())
	}

	if *checkPath != "" {
		ref, err := loadNavComparison(*checkPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench nav: load --check report: %v\n", err)
			os.Exit(1)
		}
		// Each lane: reach may not fall >2 pts, mean calls/tokens may not rise >5%
		// (map metrics prefixed map_); plus the map's token advantage may not erode.
		regs := cmp.Regressions(ref, 0.02, 0.05)
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

// emitNavReport prints a single no-map report (the fallback when the map lane
// is unavailable), preserving the pre-Phase-B output shape.
func emitNavReport(rep benchnav.Report, format string) {
	if format == "json" {
		out, err := rep.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dex bench nav: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
		return
	}
	fmt.Print(rep.Markdown())
}

// buildNavMapModel constructs the orientation map the seeded policy navigates
// with, from the live Louvain communities — the SAME L0/L1 `dex map` renders by
// default (min-members 3, top-k 25). L0Tokens prices the one orientation call;
// Locate reports whether a gold file is named in an L0-shown cluster's rendered
// L1 (so budget truncation is honored exactly as the agent sees it) and that
// zoom's token cost.
func buildNavMapModel(ctx context.Context, root string, l0budget, l1budget int) (benchnav.MapModel, error) {
	base, err := indexDir()
	if err != nil {
		return benchnav.MapModel{}, err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.GraphCommunities(ctx, mcp.CommunitiesInput{MinMembers: 3, TopK: 25, ProjectRoot: root})
	if err != nil {
		return benchnav.MapModel{}, err
	}
	if out.Status != "ok" {
		return benchnav.MapModel{}, fmt.Errorf("graph communities status %q", out.Status)
	}
	clusters := adaptCommunities(out.Communities)
	if len(clusters) == 0 {
		return benchnav.MapModel{}, fmt.Errorf("no clusters in graph")
	}

	// Pre-render the L1 of each L0-shown cluster once; Locate scans these texts.
	type shownL1 struct {
		text   string
		tokens int
	}
	var shown []shownL1
	for _, c := range codemap.ShownL0(clusters, l0budget) {
		txt := codemap.RenderL1(c, l1budget)
		shown = append(shown, shownL1{text: txt, tokens: tokens.Count(txt)})
	}
	l0tokens := tokens.Count(codemap.RenderL0(clusters, l0budget))

	return benchnav.MapModel{
		L0Tokens: l0tokens,
		Locate: func(path string) (int, bool) {
			best, found := 0, false
			for _, sc := range shown {
				if strings.Contains(sc.text, path) && (!found || sc.tokens < best) {
					best, found = sc.tokens, true
				}
			}
			return best, found
		},
	}, nil
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

func loadNavComparison(path string) (benchnav.Comparison, error) {
	var cmp benchnav.Comparison
	b, err := os.ReadFile(path)
	if err != nil {
		return cmp, err
	}
	return cmp, json.Unmarshal(b, &cmp)
}
