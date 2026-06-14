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

	"github.com/alehatsman/dex/internal/bench/nav"
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

func runBenchNav(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench nav", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchNavUsage) }

	goldenPath := fs.String("golden", "", "golden-set JSON path")
	k := fs.Int("k", 10, "read horizon")
	lane := fs.String("lane", "full", "retrieval lane: full | bm25 | onnx")
	l0budget := fs.Int("l0-budget", 0, "map-lane L0 token budget (default 150)")
	l1budget := fs.Int("l1-budget", 0, "map-lane L1 token budget per cluster (default 1000)")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference report JSON to check for regression")
	classify := fs.Bool("classify", true, "partition queries by dependency class G1/G2/G3 (#549): "+
		"G1 reached by the no-map lane, G2 only by the map lane, G3 by neither (hidden). "+
		"Use --lane=bm25 for strict CodeCompass keyword-findability. Golden `class` tags override.")

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
		return flag.ErrHelp
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	p, err := resolveEvalProject(projectPath)
	if err != nil {
		return fmt.Errorf("dex bench nav: %w", err)
	}

	gPath := *goldenPath
	if gPath == "" {
		gPath = filepath.Join(p.Root, "benchmark", "eval", "golden.json")
	}
	gs, err := eval.LoadGolden(gPath)
	if err != nil {
		return fmt.Errorf("dex bench nav: load golden set: %w\n  (generate one with: dex bench eval %s --gen)", err, projectPath)
	}
	if len(gs.Queries) == 0 {
		return fmt.Errorf("dex bench nav: golden set is empty")
	}

	if _, err := os.Stat(p.DBPath); err != nil {
		return fmt.Errorf("dex bench nav: no index for %s — run `dex index %s` first", p.Root, p.Root)
	}
	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		return fmt.Errorf("dex bench nav: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	stats, _ := st.Stats(ctx)
	em, err := evalEmbedForLane(*lane, stats.EmbedModel)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "dex bench nav: %d queries, k=%d, lane=%s, index %s\n", len(gs.Queries), *k, *lane, p.DBPath)

	results, err := eval.RunWithRewrite(ctx, em, st, gs, *k, nil)
	if err != nil {
		return fmt.Errorf("dex bench nav: run: %w", err)
	}

	queries := make([]nav.Query, 0, len(results))
	for _, r := range results {
		queries = append(queries, nav.Query{
			Query:    r.Query,
			Ranked:   r.RankedFiles,
			Relevant: r.Relevant,
			Class:    r.Class, // manual golden tag; auto-classified below when empty
		})
	}

	cost := navCostModel(p.Root)
	noMap := nav.Compute(queries, *k, cost, *lane)

	// Phase B: seed navigation with `dex map`. Build the same L0/L1 the agent
	// would see by default, then measure the map-vs-no-map delta. If the graph has
	// no communities (e.g. BM25-only index), report the no-map lane alone.
	mapModel, routeModel, breadthModel, mapErr := buildNavMapModel(ctx, p.Root, *l0budget, *l1budget, navRoutingBudgets)
	if mapErr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: map lane unavailable (%v) — reporting no-map only\n", mapErr)
		if *classify {
			// No graph reach available: queries split into G1 (lexically found)
			// and G3 (hidden) only — G2 is unobservable without the map lane.
			autoClassifyQueries(queries, navReachFlags(noMap), nil)
			noMap = nav.Compute(queries, *k, cost, *lane)
		}
		return emitNavReport(noMap, *outputFmt)
	}
	mapSeeded := nav.ComputeMap(queries, cost, mapModel, *lane)
	if *classify {
		// Auto-classify for free from the reach we already computed: no-map reach
		// is the lexical signal (G1), map-only reach is G2, neither is G3 (#549).
		// Recompute both lanes so each report carries the per-class breakdown.
		autoClassifyQueries(queries, navReachFlags(noMap), navReachFlags(mapSeeded))
		noMap = nav.Compute(queries, *k, cost, *lane)
		mapSeeded = nav.ComputeMap(queries, cost, mapModel, *lane)
	}
	cmp := nav.Compare(noMap, mapSeeded)
	cmp.Routing = nav.ComputeRouting(queries, routeModel, navRoutingBudgets, *lane)
	if tasks, around, terr := buildBreadthTasks(ctx, st, em, *k, *l1budget); terr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: breadth lane unavailable (%v) — skipping\n", terr)
	} else {
		cmp.Breadth = nav.ComputeBreadth(tasks, *k, cost, breadthModel, around, *lane)
	}
	// Re-orientation lane (#351 phase 3): restore a session's working set after
	// compaction via recap() vs re-exploration. Built purely from the no-map
	// queries already run — no extra retrieval — plus a graph-skeleton recap cost.
	if rtasks := buildReorientTasks(queries, *k); len(rtasks) == 0 {
		fmt.Fprintf(os.Stderr, "dex bench nav: reorient lane — no sessions with a reachable working set, skipping\n")
	} else if rm, rerr := buildRecapModel(ctx, st); rerr != nil {
		fmt.Fprintf(os.Stderr, "dex bench nav: reorient lane unavailable (%v) — skipping\n", rerr)
	} else {
		fmt.Fprintf(os.Stderr, "dex bench nav: reorient lane — %d sessions of %d queries, recap budget %d tokens\n",
			len(rtasks), navReorientSessionSize, navReorientRecapBudget)
		cmp.Reorient = nav.ComputeReorient(rtasks, *k, cost, rm, *lane)
	}

	switch *outputFmt {
	case "json":
		out, err := cmp.JSON()
		if err != nil {
			return fmt.Errorf("dex bench nav: marshal: %w", err)
		}
		fmt.Println(string(out))
	default:
		fmt.Print(cmp.Markdown())
	}

	if *checkPath != "" {
		ref, err := loadNavComparison(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench nav: load --check report: %w", err)
		}
		// Each lane: reach may not fall >2 pts, mean calls/tokens may not rise >5%
		// (map metrics prefixed map_); plus the map's token advantage may not erode.
		regs := cmp.Regressions(ref, 0.02, 0.05)
		if len(regs) > 0 {
			fmt.Fprintln(os.Stderr, "dex bench nav: regression check FAILED:")
			for _, r := range regs {
				fmt.Fprintf(os.Stderr, "  - %s\n", r)
			}
			return fmt.Errorf("dex bench nav: regression check failed (%d regression(s))", len(regs))
		}
		fmt.Fprintln(os.Stderr, "dex bench nav: regression check passed")
	}
	return nil
}

// navReachFlags extracts per-query reach (1:1 with the report's queries) so a
// lane's reach can seed dependency-class partitioning (#549).
func navReachFlags(r nav.Report) []bool {
	out := make([]bool, len(r.Results))
	for i, res := range r.Results {
		out[i] = res.Reached
	}
	return out
}

// autoClassifyQueries stamps an empty Class on each query from its lane reach
// (#549): G1 if the no-map (lexical) lane reached the gold, else G2 if the map
// lane did, else G3 (hidden — no static lane reaches it). Manual golden `class`
// tags are left untouched. graphReached may be nil when the map lane is
// unavailable, collapsing the partition to G1/G3.
func autoClassifyQueries(queries []nav.Query, lexReached, graphReached []bool) {
	for i := range queries {
		if queries[i].Class != "" {
			continue
		}
		lex := i < len(lexReached) && lexReached[i]
		graph := i < len(graphReached) && graphReached[i]
		queries[i].Class = nav.Classify(lex, graph)
	}
}

// emitNavReport prints a single no-map report (the fallback when the map lane
// is unavailable), preserving the pre-Phase-B output shape.
func emitNavReport(rep nav.Report, format string) error {
	if format == "json" {
		out, err := rep.JSON()
		if err != nil {
			return fmt.Errorf("dex bench nav: marshal: %w", err)
		}
		fmt.Println(string(out))
		return nil
	}
	fmt.Print(rep.Markdown())
	return nil
}

// buildNavMapModel constructs the orientation map the seeded policy navigates
// with, from the live Louvain communities — the SAME L0/L1 `dex map` renders by
// default (min-members 3, top-k 25). L0Tokens prices the one orientation call;
// Locate reports whether a gold file is named in an L0-shown cluster's rendered
// L1 (so budget truncation is honored exactly as the agent sees it) and that
// zoom's token cost.
func buildNavMapModel(ctx context.Context, root string, l0budget, l1budget int, routingBudgets []int) (nav.MapModel, nav.RoutingModel, nav.BreadthModel, error) {
	base, err := indexDir()
	if err != nil {
		return nav.MapModel{}, nav.RoutingModel{}, nav.BreadthModel{}, err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.GraphCommunities(ctx, mcp.CommunitiesInput{MinMembers: 3, TopK: 25, ProjectRoot: root})
	if err != nil {
		return nav.MapModel{}, nav.RoutingModel{}, nav.BreadthModel{}, err
	}
	if out.Status != "ok" {
		return nav.MapModel{}, nav.RoutingModel{}, nav.BreadthModel{}, fmt.Errorf("graph communities status %q", out.Status)
	}
	clusters := adaptCommunities(out.Communities)
	if len(clusters) == 0 {
		return nav.MapModel{}, nav.RoutingModel{}, nav.BreadthModel{}, fmt.Errorf("no clusters in graph")
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
	mapModel := nav.MapModel{
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
	routeModel := nav.RoutingModel{
		Routable: func(path string, budget int) bool {
			return routable[budget][path]
		},
	}

	// Breadth (issue #351 phase 2): the map's enumeration model. Cluster reports
	// the cheapest L0-shown cluster whose rendered L1 names a path (honoring L1
	// truncation, like Locate) plus that cluster's id, so distinct zooms are
	// charged once when a task's targets share a region.
	breadthModel := nav.BreadthModel{
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
func navCostModel(root string) nav.CostModel {
	cache := make(map[string]int)
	return nav.CostModel{
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

func loadNavComparison(path string) (nav.Comparison, error) {
	var cmp nav.Comparison
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
func buildBreadthTasks(ctx context.Context, st *store.Store, em embed.Embedder, k, l1budget int) ([]nav.BreadthTask, nav.AroundModel, error) {
	nodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		return nil, nav.AroundModel{}, err
	}
	idFile := make(map[string]string, len(nodes))
	idQual := make(map[string]string, len(nodes))
	isHub := make(map[string]bool, len(nodes))
	// idSym carries the fields the exact (around) lane needs to render a seed's
	// neighborhood the way `dex map --around` does (#356).
	idSym := make(map[string]codemap.Symbol, len(nodes))
	for _, n := range nodes {
		if n.FilePath == "" {
			continue
		}
		idFile[n.ID] = n.FilePath
		idQual[n.ID] = n.QualifiedName
		idSym[n.ID] = codemap.Symbol{
			QualifiedName: n.QualifiedName,
			Kind:          n.Kind,
			Pkg:           n.PackagePath,
			Path:          n.FilePath,
			Line:          n.StartLine,
			PageRank:      n.PageRank,
		}
		if n.Kind == "function" || n.Kind == "method" {
			isHub[n.ID] = true
		}
	}

	edges, err := st.GraphAllEdges(ctx)
	if err != nil {
		return nil, nav.AroundModel{}, err
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
		id    string
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
		seeds = append(seeds, seedTask{id: id, query: query, label: "neighborhood of " + qual, files: files})
	}
	if len(seeds) == 0 {
		return nil, nav.AroundModel{}, fmt.Errorf("no hub symbols with >= %d neighbor files", navBreadthMinNeighbors)
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

	// Exact (around) lane: render each seed's callers ∪ callees neighborhood the
	// way `dex map --around <query>` does (#347), keyed by the task label so the
	// breadth model reads coverage off the same budgeted text the agent sees.
	// Built from the same edge set the targets were derived from, so the region
	// IS the neighborhood — coverage is bounded only by the L1 token budget.
	aroundText := make(map[string]string, len(seeds))
	aroundTok := make(map[string]int, len(seeds))
	for _, sd := range seeds {
		syms := make([]codemap.Symbol, 0, len(adj[sd.id]))
		for nb := range adj[sd.id] {
			if s, ok := idSym[nb]; ok {
				syms = append(syms, s)
			}
		}
		if len(syms) == 0 {
			continue
		}
		text := codemap.RenderAround(mcp.AroundTitle(sd.query), syms, l1budget)
		aroundText[sd.label] = text
		aroundTok[sd.label] = tokens.Count(text)
	}
	around := nav.AroundModel{Region: func(task string) (string, int, bool) {
		text, ok := aroundText[task]
		if !ok {
			return "", 0, false
		}
		return text, aroundTok[task], true
	}}

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
		return nil, nav.AroundModel{}, err
	}
	tasks := make([]nav.BreadthTask, 0, len(results))
	for _, r := range results {
		sd, ok := byQuery[r.Query]
		if !ok {
			continue
		}
		tasks = append(tasks, nav.BreadthTask{
			Task:    sd.label,
			Targets: sd.files,
			Ranked:  r.RankedFiles,
		})
	}
	return tasks, around, nil
}

// navReorientSessionSize is how many consecutive golden queries form one
// synthetic work session whose working set the re-orientation lane restores.
// Larger sessions mean larger working sets — more find()s for re-exploration to
// replay and more files for recap to fit, which is where recap's one-call edge
// shows. navReorientRecapBudget caps the single recap() digest (#346 budget):
// big enough for a typical working set, tight enough that oversized sessions
// truncate so recap coverage becomes a real, gateable signal.
const (
	navReorientSessionSize = 5
	navReorientRecapBudget = 4000
)

// buildReorientTasks groups the already-run no-map queries into work sessions
// and derives each session's working set — the gold files reachable within k,
// i.e. what the agent had open before compaction. No extra retrieval: it is a
// pure transform of the queries the no-map lane already ranked. Sessions whose
// working set is empty (no gold reachable across the bucket) are dropped.
func buildReorientTasks(queries []nav.Query, k int) []nav.ReorientTask {
	var tasks []nav.ReorientTask
	for start := 0; start < len(queries); start += navReorientSessionSize {
		end := start + navReorientSessionSize
		if end > len(queries) {
			end = len(queries)
		}
		seen := map[string]bool{}
		var working []string
		var rqs []nav.ReorientQuery
		for _, q := range queries[start:end] {
			depth := k
			if len(q.Ranked) < depth {
				depth = len(q.Ranked)
			}
			rel := map[string]bool{}
			for _, r := range q.Relevant {
				rel[r] = true
			}
			var gold []string
			for i := 0; i < depth; i++ {
				p := q.Ranked[i]
				if !rel[p] {
					continue
				}
				gold = append(gold, p)
				if !seen[p] {
					seen[p] = true
					working = append(working, p)
				}
			}
			if len(gold) == 0 {
				continue // this find() re-surfaces nothing the agent had kept
			}
			rqs = append(rqs, nav.ReorientQuery{Ranked: q.Ranked, Gold: gold})
		}
		if len(working) == 0 {
			continue
		}
		tasks = append(tasks, nav.ReorientTask{
			Task:    fmt.Sprintf("session %d (queries %d-%d)", len(tasks)+1, start+1, end),
			Working: working,
			Queries: rqs,
		})
	}
	return tasks
}

// buildRecapModel prices recap()'s digest from the graph: one working-set file
// costs its path plus the symbol names it defines (a compressed signature
// skeleton — restore WHERE you were, not the full file). Files with no graph
// symbols fall back to the path line alone. This prices the same entry the live
// recap() now renders (the session `recap` action, internal/mcp/server_session.go
// recapEntryText) — the gate measures what ships — mirroring how the map/breadth
// lanes model their verb without the live stack.
func buildRecapModel(ctx context.Context, st *store.Store) (nav.ReorientModel, error) {
	nodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		return nav.ReorientModel{}, err
	}
	symbols := map[string][]string{}
	for _, n := range nodes {
		if n.FilePath == "" || n.QualifiedName == "" {
			continue
		}
		symbols[n.FilePath] = append(symbols[n.FilePath], n.QualifiedName)
	}
	entry := func(path string) int {
		var b strings.Builder
		b.WriteString(path)
		for _, s := range symbols[path] {
			b.WriteString("\n")
			b.WriteString(s)
		}
		return tokens.Count(b.String())
	}
	return nav.ReorientModel{RecapBudget: navReorientRecapBudget, Entry: entry}, nil
}
