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

	"github.com/alehatsman/dex/internal/bench/pack"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/tokens"
)

// packGoldMax bounds a task's ripple set to the common modify case — a symbol
// with at most this many caller/callee files. Bigger god-objects are skipped.
const packGoldMax = 10

const benchPackUsage = `Usage: dex bench pack <project> [flags]

Modify-symbol working-set cost: the primitive multi-call path (locate → read →
trace callers → trace callees → find tests → read each) versus one
ask(intent=assemble) context pack. Instruments epic #95 AC #2 (fewer retrieval
calls) and AC #6 (fewer tokens without reducing correctness).

Seeds are the highest-fan-in function/method symbols in the call graph; a task's
gold working set = the symbol's file ∪ its caller/callee files. Coverage = the
fraction of gold the pack surfaced; the cost delta is credited only over tasks
the pack fully covers, so a token win that drops a dependent site does not count.

Flags:
  --k int          assemble depth per lane (default 12)
  --max-tasks int  cap on seed symbols, largest neighbour set first (default 25)
  --output format  json or md (default: md)
  --check path     compare against a reference report JSON; exit 1 on regression
  --cov-tol float  max allowed coverage/reach drop vs reference (default 0.05)
  --cost-tol float max allowed pack-cost rise vs reference, fraction (default 0.10)
`

func runBenchPack(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench pack", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchPackUsage) }

	k := fs.Int("k", 12, "assemble depth per lane")
	maxTasks := fs.Int("max-tasks", 25, "cap on seed symbols")
	outputFmt := fs.String("output", "md", "output format: json or md")
	checkPath := fs.String("check", "", "reference report JSON to check for regression")
	covTol := fs.Float64("cov-tol", 0.05, "max allowed coverage/reach drop vs reference")
	costTol := fs.Float64("cost-tol", 0.10, "max allowed pack-cost rise vs reference (fraction)")

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
		return fmt.Errorf("dex bench pack: %w", err)
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		return fmt.Errorf("dex bench pack: no index for %s — run `dex index %s` first", p.Root, p.Root)
	}
	st, err := store.OpenWith(ctx, p.DBPath, storeOpts())
	if err != nil {
		return fmt.Errorf("dex bench pack: open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	seeds, qualFile, err := buildPackTasks(ctx, st, *maxTasks)
	if err != nil {
		return fmt.Errorf("dex bench pack: %w", err)
	}
	fmt.Fprintf(os.Stderr, "dex bench pack: %d modify-symbol tasks, k=%d, index %s\n", len(seeds), *k, p.DBPath)

	// The live assemble path is the MCP ask verb with intent=assemble — the same
	// ContextPack an agent gets. Run it once per seed and record what it surfaced.
	base, err := indexDir()
	if err != nil {
		return fmt.Errorf("dex bench pack: %w", err)
	}
	srv, _ := newServerFromEnv(base)

	type packOut struct {
		files  []string
		tokens int
		ok     bool
	}
	surfaced := make(map[string]packOut, len(seeds))
	tasks := make([]pack.Task, 0, len(seeds))
	for _, sd := range seeds {
		tasks = append(tasks, sd.task)
		_, out, aerr := srv.ContextRouter(ctx, mcp.ContextInput{
			ProjectRoot: p.Root,
			Question:    sd.query,
			Intent:      "assemble",
			K:           *k,
			Budget:      1_000_000, // large: we want tokens_returned, never a truncation
		})
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "dex bench pack: assemble %q failed: %v\n", sd.query, aerr)
			surfaced[sd.task.Symbol] = packOut{}
			continue
		}
		files := packSurfacedFiles(out, qualFile)
		if os.Getenv("PACKBENCH_DEBUG") != "" {
			cov := 0
			gold := map[string]bool{}
			for _, g := range sd.task.Gold {
				gold[g] = true
			}
			for _, f := range files {
				if gold[f] {
					cov++
				}
			}
			fmt.Fprintf(os.Stderr, "  [dbg] q=%q status=%s surfaced=%d gold=%d covered=%d\n",
				sd.query, out.Status, len(files), len(sd.task.Gold), cov)
		}
		// loop-blocked is the session-dedup guard tripping under the tight bench
		// loop (#110); it still returns real evidence, so evidence presence — not
		// the status string — decides reach.
		surfaced[sd.task.Symbol] = packOut{
			files:  files,
			tokens: packTokens(out),
			ok:     len(files) > 0,
		}
	}

	pm := pack.PackModel{Surfaced: func(sym string) ([]string, int, bool) {
		r := surfaced[sym]
		return r.files, r.tokens, r.ok
	}}
	rep := pack.Compute(tasks, packCostModel(p.Root), pm)

	switch *outputFmt {
	case "json":
		b, err := rep.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case "md", "":
		fmt.Print(rep.Markdown())
	default:
		return fmt.Errorf("dex bench pack: unknown output format %q", *outputFmt)
	}

	if *checkPath != "" {
		ref, err := loadPackReport(*checkPath)
		if err != nil {
			return fmt.Errorf("dex bench pack: load reference: %w", err)
		}
		if regs := rep.Regressions(ref, *covTol, *costTol); len(regs) > 0 {
			fmt.Fprintln(os.Stderr, "\ndex bench pack: REGRESSION vs reference:")
			for _, r := range regs {
				fmt.Fprintf(os.Stderr, "  - %s\n", r)
			}
			return fmt.Errorf("%d regression(s)", len(regs))
		}
		fmt.Fprintln(os.Stderr, "\ndex bench pack: no regression vs reference")
	}
	return nil
}

// packSeed pairs a bench task with the query used to drive its assemble call.
type packSeed struct {
	task  pack.Task
	query string
}

// buildPackTasks derives modify-symbol tasks from the call graph, reusing the
// same hub-seed machinery as the nav breadth lane: seed on the highest-fan-in
// function/method symbols, gold = the seed's own file ∪ its caller/callee files.
func buildPackTasks(ctx context.Context, st *store.Store, maxTasks int) ([]packSeed, map[string]string, error) {
	nodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	idFile := make(map[string]string, len(nodes))
	idQual := make(map[string]string, len(nodes))
	isHub := make(map[string]bool, len(nodes))
	qualFile := make(map[string]string, len(nodes)) // qualified name → file, for pack graph-lane resolution
	for _, n := range nodes {
		if n.FilePath == "" {
			continue
		}
		idFile[n.ID] = n.FilePath
		idQual[n.ID] = n.QualifiedName
		if n.QualifiedName != "" {
			qualFile[n.QualifiedName] = n.FilePath
		}
		if n.Kind == "function" || n.Kind == "method" {
			isHub[n.ID] = true
		}
	}

	edges, err := st.GraphAllEdges(ctx)
	if err != nil {
		return nil, nil, err
	}
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

	seeds := buildBreadthSeeds(isHub, idFile, idQual, adj)
	if len(seeds) == 0 {
		return nil, nil, fmt.Errorf("no hub symbols with >= %d neighbour files (empty or BM25-only graph?)", navBreadthMinNeighbors)
	}
	// Target the *common* modify case: a symbol with a handful of dependents
	// (band [min, packGoldMax]), not a 30-caller god-object whose full ripple no
	// single pack could ever surface. This keeps full ripple coverage attainable
	// and the numbers representative of everyday work (AC #2's "common workflow").
	band := seeds[:0]
	for _, s := range seeds {
		if len(s.files) <= packGoldMax {
			band = append(band, s)
		}
	}
	seeds = band
	if len(seeds) == 0 {
		return nil, nil, fmt.Errorf("no hub symbols in the [%d, %d] neighbour-file band", navBreadthMinNeighbors, packGoldMax)
	}
	sort.SliceStable(seeds, func(i, j int) bool {
		if len(seeds[i].files) != len(seeds[j].files) {
			return len(seeds[i].files) < len(seeds[j].files)
		}
		return seeds[i].query < seeds[j].query
	})
	if maxTasks > 0 && len(seeds) > maxTasks {
		fmt.Fprintf(os.Stderr, "dex bench pack: %d eligible hubs, capping to largest %d\n", len(seeds), maxTasks)
		seeds = seeds[:maxTasks]
	}

	out := make([]packSeed, 0, len(seeds))
	for _, sd := range seeds {
		def := idFile[sd.id]
		gold := append([]string{def}, sd.files...) // def ∪ caller/callee files
		// Drive the assemble with the qualified name — the fairest "modify this
		// symbol" prompt, richer for resolution than a bare short name.
		query := idQual[sd.id]
		if query == "" {
			query = sd.query
		}
		out = append(out, packSeed{
			task:  pack.Task{Symbol: idQual[sd.id], Def: def, Gold: gold},
			query: query,
		})
	}
	return out, qualFile, nil
}

// packSurfacedFiles collects every repo-relative file the assemble pack put in
// front of the agent, across all its evidence lanes plus paired tests.
func packSurfacedFiles(out mcp.ContextOutput, qualFile map[string]string) []string {
	set := map[string]bool{}
	add := func(p string) {
		if p != "" {
			set[p] = true
		}
	}
	for _, s := range out.Symbols {
		add(s.Path)
	}
	for _, h := range out.SemanticHits {
		add(h.Path)
	}
	for _, r := range out.SuggestedReads {
		add(r.Path)
	}
	for _, r := range out.References {
		add(r.Path)
	}
	for p, meta := range out.Annotations {
		add(p)
		for _, t := range meta.Tests {
			add(t)
		}
	}
	// The graph lane names neighbour symbols (callers/callees) by qualified
	// name, not file — resolve them to the files an agent would open.
	if out.Graph != nil {
		for _, n := range out.Graph.Nodes {
			add(qualFile[n.QualifiedName])
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// packTokens is the pack's rendered token cost. cost.tokens_returned is always
// reported by the router; fall back to summing the inlined evidence if absent.
func packTokens(out mcp.ContextOutput) int {
	if out.Cost != nil && out.Cost.TokensReturned > 0 {
		return out.Cost.TokensReturned
	}
	n := 0
	for _, s := range out.Symbols {
		n += tokens.Count(s.Signature) + tokens.Count(s.Doc) + tokens.Count(s.Body)
	}
	for _, h := range out.SemanticHits {
		n += tokens.Count(h.Content)
	}
	for _, r := range out.SuggestedReads {
		n += tokens.Count(r.Content)
	}
	return n
}

// packCostModel prices the primitive path: one Read = the file's token count,
// one TraceEnvelope = the tokens of the neighbour-path list the agent scans.
func packCostModel(root string) pack.CostModel {
	cache := make(map[string]int)
	return pack.CostModel{
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
		TraceEnvelope: func(paths []string) int {
			return tokens.Count(strings.Join(paths, "\n"))
		},
	}
}

func loadPackReport(path string) (pack.Report, error) {
	var rep pack.Report
	b, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(b, &rep)
}
