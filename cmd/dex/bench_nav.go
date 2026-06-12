package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/benchnav"
	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/embed"
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

// navRoutingBudgets is the L0-budget sweep for routing accuracy@budget (issue
// #351). Anchored on codemap.DefaultL0Budget (150); the spread shows how L0
// breadth trades against orientation coverage — the curve stories #347/#348
// must lift. Accuracy is monotonic non-decreasing across this sweep.
var navRoutingBudgets = []int{75, 150, 300, 600, 1200}

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
	mapModel, routeModel, breadthModel, mapErr := buildNavMapModel(ctx, p.Root, *l0budget, *l1budget, navRoutingBudgets)
	if mapErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: map lane unavailable (%v) — reporting no-map only\n", mapErr)
		emitNavReport(noMap, *outputFmt)
		return
	}
	mapSeeded := benchnav.ComputeMap(queries, cost, mapModel, *lane)
	cmp := benchnav.Compare(noMap, mapSeeded)
	cmp.Routing = benchnav.ComputeRouting(queries, routeModel, navRoutingBudgets, *lane)
	if tasks, terr := buildBreadthTasks(ctx, st, em, *k); terr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: breadth lane unavailable (%v) — skipping\n", terr)
	} else {
		cmp.Breadth = benchnav.ComputeBreadth(tasks, *k, cost, breadthModel, *lane)
	}

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
func buildNavMapModel(ctx context.Context, root string, l0budget, l1budget int, routingBudgets []int) (benchnav.MapModel, benchnav.RoutingModel, benchnav.BreadthModel, error) {
	base, err := indexDir()
	if err != nil {
		return benchnav.MapModel{}, benchnav.RoutingModel{}, benchnav.BreadthModel{}, err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.GraphCommunities(ctx, mcp.CommunitiesInput{MinMembers: 3, TopK: 25, ProjectRoot: root})
	if err != nil {
		return benchnav.MapModel{}, benchnav.RoutingModel{}, benchnav.BreadthModel{}, err
	}
	if out.Status != "ok" {
		return benchnav.MapModel{}, benchnav.RoutingModel{}, benchnav.BreadthModel{}, fmt.Errorf("graph communities status %q", out.Status)
	}
	clusters := adaptCommunities(out.Communities)
	if len(clusters) == 0 {
		return benchnav.MapModel{}, benchnav.RoutingModel{}, benchnav.BreadthModel{}, fmt.Errorf("no clusters in graph")
	}

	// Pre-render the L1 of each L0-shown cluster once; Locate scans these texts.
	type shownL1 struct {
		id     int
		text   string
		tokens int
	}
	var shown []shownL1
	for _, c := range codemap.ShownL0(clusters, l0budget) {
		txt := codemap.RenderL1(c, l1budget)
		shown = append(shown, shownL1{id: c.ID, text: txt, tokens: tokens.Count(txt)})
	}
	l0tokens := tokens.Count(codemap.RenderL0(clusters, l0budget))
	mapModel := benchnav.MapModel{
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
	}

	// Routing (issue #351): precompute, per sweep budget, the file paths that are
	// members of the L0-shown clusters — the region one map(budget) call surfaces.
	// Routable is then a set lookup, so cost is independent of the query count, and
	// it is NOT confounded by L1 truncation (routing is an L0-only question).
	routable := make(map[int]map[string]bool, len(routingBudgets))
	for _, b := range routingBudgets {
		set := make(map[string]bool)
		for _, c := range codemap.ShownL0(clusters, b) {
			for _, sym := range c.Symbols {
				set[sym.Path] = true
			}
		}
		routable[b] = set
	}
	routeModel := benchnav.RoutingModel{
		Routable: func(path string, budget int) bool {
			return routable[budget][path]
		},
	}

	// Breadth (issue #351 phase 2): the map's enumeration model. Cluster reports
	// the cheapest L0-shown cluster whose rendered L1 names a path (honoring L1
	// truncation, like Locate) plus that cluster's id, so distinct zooms are
	// charged once when a task's targets share a region.
	breadthModel := benchnav.BreadthModel{
		L0Tokens: l0tokens,
		Cluster: func(path string) (int, int, bool) {
			id, best, found := 0, 0, false
			for _, sc := range shown {
				if strings.Contains(sc.text, path) && (!found || sc.tokens < best) {
					id, best, found = sc.id, sc.tokens, true
				}
			}
			return id, best, found
		},
	}
	return mapModel, routeModel, breadthModel, nil
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

// navBreadthMinNeighbors is the smallest neighbor set that counts as a breadth
// task — a hub with fewer distinct neighbor files is not a multi-target problem.
const navBreadthMinNeighbors = 4

// navBreadthMaxTasks caps how many hub symbols become breadth tasks (highest
// neighbor-file count first); the cap is logged, never silent.
const navBreadthMaxTasks = 25

// buildBreadthTasks derives multi-target navigation tasks from the call graph:
// seed on the highest-fan-in function/method symbols, target set = the FULL
// (uncapped) set of files holding the seed's callers ∪ callees, ground truth
// from the graph edges. This is the map's claimed regime — communities are built
// from those same edges, so a hub's neighbors cluster together and one L1 zoom
// enumerates them, whereas find() ranks by similarity, not adjacency. One find()
// per seed (over its short name) populates the no-map lane's ranking. Seeds are
// taken by descending neighbor-file count and capped to bound runtime.
func buildBreadthTasks(ctx context.Context, st *store.Store, em embed.Embedder, k int) ([]benchnav.BreadthTask, error) {
	nodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		return nil, err
	}
	idFile := make(map[string]string, len(nodes))
	idQual := make(map[string]string, len(nodes))
	isHub := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.FilePath == "" {
			continue
		}
		idFile[n.ID] = n.FilePath
		idQual[n.ID] = n.QualifiedName
		if n.Kind == "function" || n.Kind == "method" {
			isHub[n.ID] = true
		}
	}

	edges, err := st.GraphAllEdges(ctx)
	if err != nil {
		return nil, err
	}
	// Undirected adjacency over node ids: callers ∪ callees.
	adj := map[string]map[string]bool{}
	link := func(a, b string) {
		if adj[a] == nil {
			adj[a] = map[string]bool{}
		}
		adj[a][b] = true
	}
	for _, e := range edges {
		if e.SrcID == "" || e.DstID == "" || e.SrcID == e.DstID {
			continue
		}
		link(e.SrcID, e.DstID)
		link(e.DstID, e.SrcID)
	}

	type seedTask struct {
		query string
		label string
		files []string
	}
	var seeds []seedTask
	usedQuery := map[string]bool{}
	for id := range isHub {
		own := idFile[id]
		fileSet := map[string]bool{}
		for nb := range adj[id] {
			f := idFile[nb]
			if f == "" || f == own {
				continue
			}
			fileSet[f] = true
		}
		if len(fileSet) < navBreadthMinNeighbors {
			continue
		}
		qual := idQual[id]
		query := qual
		if idx := strings.LastIndexAny(qual, "./"); idx >= 0 && idx < len(qual)-1 {
			query = qual[idx+1:]
		}
		if query == "" || usedQuery[query] {
			continue // keep find queries unique so results map back cleanly
		}
		usedQuery[query] = true
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)
		seeds = append(seeds, seedTask{query: query, label: "neighborhood of " + qual, files: files})
	}
	if len(seeds) == 0 {
		return nil, fmt.Errorf("no hub symbols with >= %d neighbor files", navBreadthMinNeighbors)
	}
	// Largest neighbor sets first; deterministic tie-break by query.
	sort.SliceStable(seeds, func(i, j int) bool {
		if len(seeds[i].files) != len(seeds[j].files) {
			return len(seeds[i].files) > len(seeds[j].files)
		}
		return seeds[i].query < seeds[j].query
	})
	if len(seeds) > navBreadthMaxTasks {
		fmt.Fprintf(os.Stderr, "dex bench nav: breadth lane — %d eligible hubs, capping to largest %d\n", len(seeds), navBreadthMaxTasks)
		seeds = seeds[:navBreadthMaxTasks]
	}

	// One synthetic golden query per seed, run through the SAME retrieval path
	// the no-map lane uses, to get each task's find() ranking. Look results back
	// up by query (unique by construction) so order assumptions don't matter.
	byQuery := make(map[string]seedTask, len(seeds))
	gs := eval.GoldenSet{}
	for _, sd := range seeds {
		byQuery[sd.query] = sd
		gs.Queries = append(gs.Queries, eval.GoldenQuery{Query: sd.query, RelevantFiles: sd.files})
	}
	results, err := eval.RunWithRewrite(ctx, em, st, gs, k, nil)
	if err != nil {
		return nil, err
	}
	tasks := make([]benchnav.BreadthTask, 0, len(results))
	for _, r := range results {
		sd, ok := byQuery[r.Query]
		if !ok {
			continue
		}
		tasks = append(tasks, benchnav.BreadthTask{
			Task:    sd.label,
			Targets: sd.files,
			Ranked:  r.RankedFiles,
		})
	}
	return tasks, nil
}
