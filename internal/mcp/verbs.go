package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The verb facade (#316 story 3): a small default tool surface — map / find /
// trace / read / ask — that everyday agents reach for, with the
// granular graph/search/analysis lanes moved behind DEX_EXPERT. The facades
// here are thin compositions over the existing toolSurface handlers (no
// handler rewrites): they route an input to the right underlying call and copy
// its result into one verb-shaped envelope. Because they are free functions
// over toolSurface, every backend (local *Server, remote proxy, maintenance,
// http) gets the verb for free — no new interface methods, no new REST routes.

// expertEnabled reports whether the power-tool tier should be registered. The
// default verb surface covers everyday work; operators opt into the raw lanes
// (deps/callers/callees/path/diff/clusters/routes/smells/status/notes/
// session) with DEX_EXPERT. Parsed leniently: any value other than the usual
// falsey strings enables it.
func expertEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_EXPERT"))) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// MapInput drives the map verb — a deterministic, zero-inference orientation
// map built from the pre-computed Louvain communities + PageRank already in the
// graph (epic #316 story 1). With no Cluster it renders the L0 overview (top
// clusters); with Cluster set it zooms into that one cluster (L1).
type MapInput struct {
	Cluster     *int   `json:"cluster,omitempty" jsonschema:"cluster id to zoom into (L1 detail); omit for the repo overview (L0)"`
	Budget      int    `json:"budget,omitempty" jsonschema:"token budget for the rendered map (default 150 for L0, 1000 for L1)"`
	MinMembers  int    `json:"min_members,omitempty" jsonschema:"min cluster size to consider (default 3)"`
	K           int    `json:"k,omitempty" jsonschema:"max clusters to scan (default 50)"`
	TopK        int    `json:"top_k,omitempty" jsonschema:"max symbols pulled per cluster (default 25)"`
	Around      string `json:"around,omitempty" jsonschema:"render a task-focused region around this symbol — its callers ∪ callees — instead of the repo overview; mutually exclusive with cluster and around_diff"`
	AroundDiff  string `json:"around_diff,omitempty" jsonschema:"render the blast radius of a git diff: the ref to diff against (e.g. 'HEAD~1'); mutually exclusive with cluster and around"`
	Task        string `json:"task,omitempty" jsonschema:"current task description — when set, every indexed file is scored against this task and returned as l0_files/l1_files/l2_count with per-file recommended_mode; requires an embed client (degrades to the normal topology map when none is wired)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// TaskFile is one entry in the task-filtered read list returned by map when
// task is set (#609). Score is the cosine similarity (boosted by git recency
// and session bounce) and Mode is the recommended read mode.
type TaskFile struct {
	Path  string  `json:"path"`
	Score float32 `json:"score"`
	Mode  string  `json:"mode"` // "full" | "signatures" | "skeleton"
}

// MapOutput carries the rendered markdown map plus a status. Map holds the same
// text a human sees from `dex map`; agents can read it directly. When task is
// set the task-filtered fields (L0Files/L1Files/L2Count/GitBoosted) are
// populated instead.
type MapOutput struct {
	Status string `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "error"
	Hint   string `json:"hint,omitempty"`
	Zoom   string `json:"zoom,omitempty"` // "orient" | "l1" | "around" | "task"
	Map    string `json:"map,omitempty"`
	// Task-filtered fields — populated when MapInput.Task is set.
	Task       string     `json:"task,omitempty"`
	L0Files    []TaskFile `json:"l0_files,omitempty"`
	L1Files    []TaskFile `json:"l1_files,omitempty"`
	L2Count    int        `json:"l2_count,omitempty"`
	GitBoosted []string   `json:"git_boosted,omitempty"`
}

// mapHandler adapts mapVerb to the SDK handler shape, capturing h.
func mapHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, MapInput) (*sdk.CallToolResult, MapOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
		return mapVerb(ctx, h, req, in)
	}
}

// Map runs the map verb for callers without an SDK request — the REST `/map`
// route. It composes over the local *Server exactly like the stdio `map` tool,
// so both transports agree.
func (s *Server) Map(ctx context.Context, in MapInput) (MapOutput, error) {
	_, out, err := mapVerb(ctx, s, nil, in)
	return out, err
}

// mapVerb composes the existing community projection (the `clusters` lane) with
// the codemap renderer — no model is called. It mirrors the assembly in
// `dex map` (cmd/dex/map.go) so the MCP verb and the CLI agree.
func mapVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	// #609: task-filtered read list. When a task is provided and the handler is
	// a *Server (so we have embed + store access), score every indexed file and
	// return L0/L1/L2 buckets with per-file recommended_mode. Degrades to the
	// normal topology map when no embedder is wired or on non-*Server surfaces.
	if in.Task != "" {
		if srv, ok := h.(*Server); ok {
			return srv.taskMap(ctx, in)
		}
	}
	// #347 story 5: task-conditioned region. `around`/`around_diff` render the
	// call-graph neighborhood of a symbol or the blast radius of a diff instead
	// of the Louvain L0/L1 overview, so they branch off before the community
	// projection. The two are mutually exclusive with each other and with
	// `cluster` (which zooms a community, a different notion of region).
	if in.Around != "" || in.AroundDiff != "" {
		if in.Around != "" && in.AroundDiff != "" {
			return nil, MapOutput{Status: "error", Hint: "around and around_diff are mutually exclusive — pass one"}, nil
		}
		if in.Cluster != nil {
			return nil, MapOutput{Status: "error", Hint: "around cannot be combined with cluster — cluster zooms a Louvain community; around renders a call-graph or diff region"}, nil
		}
		return mapAround(ctx, h, req, in)
	}

	minMembers, k, topK := in.MinMembers, in.K, in.TopK
	if minMembers == 0 {
		minMembers = 3
	}
	if k == 0 {
		k = 50
	}
	if topK == 0 {
		topK = 25
	}
	_, comm, err := h.graphCommunities(ctx, req, CommunitiesInput{
		MinMembers:  minMembers,
		K:           k,
		TopK:        topK,
		ProjectRoot: in.ProjectRoot,
	})
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: err.Error()}, err
	}
	if comm.Status != "ok" {
		return nil, MapOutput{Status: comm.Status, Hint: comm.Hint}, nil
	}
	if len(comm.Communities) == 0 && comm.Hint != "" {
		return nil, MapOutput{Status: "ok", Hint: comm.Hint}, nil
	}

	clusters := AdaptCommunities(comm.Communities)
	if in.Cluster != nil {
		c, ok := findCluster(clusters, *in.Cluster)
		if !ok {
			return nil, MapOutput{Status: "not-found", Hint: fmt.Sprintf("cluster #%d not found (omit `cluster` to list clusters)", *in.Cluster)}, nil
		}
		return nil, MapOutput{Status: "ok", Zoom: "l1", Map: codemap.RenderL1(c, in.Budget)}, nil
	}
	// Default (no cluster): the first-touch orientation bundle — L0 overview plus
	// an auto-zoom into the most-central cluster (#574, the former `orient`).
	// RenderOrient defaults the budgets when zero (150 L0, 1000 L1).
	return nil, MapOutput{Status: "ok", Zoom: "orient", Map: codemap.RenderOrient(clusters,
		codemap.OrientExtras{Entrypoints: comm.Entrypoints, ImportEdges: CodemapImportEdges(comm.ImportEdges), Externals: comm.Externals, Scale: CodemapScale(comm.Scale)}, in.Budget, in.Budget)}, nil
}

// AdaptCommunities maps the MCP community projection into the renderer's input.
// cmd/dex delegates here so the logic lives in one place; it cannot live in
// codemap because codemap is imported by mcp, which would create a cycle.
func AdaptCommunities(comms []Community) []codemap.Cluster {
	clusters := make([]codemap.Cluster, 0, len(comms))
	for _, c := range comms {
		syms := make([]codemap.Symbol, 0, len(c.Members))
		for _, m := range c.Members {
			syms = append(syms, codemap.Symbol{
				QualifiedName: m.QualifiedName,
				Kind:          m.Kind,
				Pkg:           m.Package,
				Path:          m.Path,
				Line:          m.StartLine,
				PageRank:      m.PageRank,
			})
		}
		clusters = append(clusters, codemap.Cluster{ID: c.ID, Size: c.Size, Symbols: syms})
	}
	return clusters
}

func findCluster(clusters []codemap.Cluster, id int) (codemap.Cluster, bool) {
	for _, c := range clusters {
		if c.ID == id {
			return c, true
		}
	}
	return codemap.Cluster{}, false
}

// TraceInput drives the trace verb — a single entry point for call-graph
// navigation from a symbol. direction selects the traversal; the call-edge
// directions (callers/callees) and path share one symbol-name input.
type TraceInput struct {
	Symbol      string `json:"symbol" jsonschema:"symbol to trace: bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	Direction   string `json:"direction,omitempty" jsonschema:"'callers' (default — who calls it), 'callees' (what it calls), 'path' (shortest call route to the 'to' symbol), or 'impact' (transitive caller blast-radius with risk tier + tests_to_run)"`
	To          string `json:"to,omitempty" jsonschema:"destination symbol; required when direction=path"`
	Package     string `json:"package,omitempty" jsonschema:"optional package-path filter when the same name is defined in multiple packages"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth limit: path (default 8, max 15); impact (default 3, max 5). Ignored for callers/callees"`
	K           int    `json:"k,omitempty" jsonschema:"max hits to return: callers/callees (default 12, max 50); impact nodes per depth (default 8, max 200)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project or git worktree you are working in. The server cannot see your shell's directory; when working in a worktree different from where the server started, pass that worktree's path"`
}

// TraceOutput is the unified envelope across the four directions. The
// call-edge directions fill Targets+Hits; path fills Src/Dst/Path; impact fills
// Nodes+MaxDepth+Total+Elided+TestsToRun. Empty fields are omitted, so each
// direction's response stays compact.
type TraceOutput struct {
	Direction string        `json:"direction"`
	Status    string        `json:"status"` // "ok" | "no-index" | "no-graph" | "not-found" | "no-path" | "error"
	Hint      string        `json:"hint,omitempty"`
	Project   string        `json:"project,omitempty"`
	Targets   []TargetMatch `json:"targets,omitempty"` // callers/callees/impact: resolved interpretations of `symbol`
	Hits      []CallSite    `json:"hits"`              // callers/callees: the call-edge endpoints
	Src       string        `json:"src,omitempty"`     // path
	Dst       string        `json:"dst,omitempty"`     // path
	Path      []PathHop     `json:"path,omitempty"`    // path: ordered hops
	// Risk is set for direction=callers and direction=impact: Low | Medium |
	// High | Critical.
	Risk string `json:"risk,omitempty"`
	// impact: the transitive caller blast-radius.
	Nodes      []ImpactNode   `json:"nodes,omitempty"`
	MaxDepth   int            `json:"max_depth,omitempty"`
	Total      int            `json:"total,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Elided     []DepthElision `json:"elided,omitempty"`
	TestsToRun []string       `json:"tests_to_run,omitempty"`
	// Recall is "partial" when the graph result may undercount call sites —
	// non-Go (tree-sitter) extractors have incomplete recall, so a non-empty
	// result is still not a complete blast radius. Check grep_hits for
	// additional candidate sites surfaced by a name-based grep sweep.
	Recall   string      `json:"recall,omitempty"`
	GrepHits []GrepMatch `json:"grep_hits,omitempty"`
	// UnresolvedInbound lists known import edges into the target symbol's package
	// that the resolver could not bind to a symbol (build-mediated / workspace
	// subpath, e.g. `@bright/common/Uuid`). Their specifier and the target's name
	// differ, so the grep sweep above cannot see them — surfacing them counted
	// turns a silent undercount into an actionable one (#130). Grep the specifier.
	UnresolvedInbound []store.UnresolvedInbound `json:"unresolved_inbound,omitempty"`
}

// traceHandler adapts traceVerb to the SDK handler shape, capturing h.
func traceHandler(h toolSurface) func(context.Context, *sdk.CallToolRequest, TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
		return traceVerb(ctx, h, req, in)
	}
}

// Trace runs the trace verb for callers without an SDK request — the REST
// `/trace` route. It composes over the local *Server exactly like the stdio
// `trace` tool, so both transports agree.
func (s *Server) Trace(ctx context.Context, in TraceInput) (TraceOutput, error) {
	_, out, err := traceVerb(ctx, s, nil, in)
	return out, err
}

// traceVerb dispatches a trace call to the underlying graph handler for the
// requested direction and folds its result into TraceOutput.
func traceVerb(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in TraceInput) (*sdk.CallToolResult, TraceOutput, error) {
	dir := strings.ToLower(strings.TrimSpace(in.Direction))
	if dir == "" {
		dir = "callers"
	}
	switch dir {
	case "callers", "callees":
		ce := CallEdgeInput{Name: in.Symbol, Package: in.Package, ProjectRoot: in.ProjectRoot, K: in.K}
		var out CallEdgeOutput
		var err error
		if dir == "callers" {
			_, out, err = h.graphCallers(ctx, req, ce)
		} else {
			_, out, err = h.graphCallees(ctx, req, ce)
		}
		tOut := TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Targets:   out.Targets,
			Hits:      out.Hits,
			Risk:      out.Risk,
		}
		if out.Status == "ok" && len(out.Hits) > 0 && hasNonGoTarget(out.Targets) {
			augmentPartialRecall(ctx, h, in.Symbol, in.ProjectRoot, &tOut)
			foldUnresolvedInbound(ctx, h, in.ProjectRoot, &tOut)
		}
		return nil, tOut, err
	case "path":
		if strings.TrimSpace(in.To) == "" {
			return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction=path requires `to` (destination symbol)"}, nil
		}
		pi := PathInput{Src: in.Symbol, Dst: in.To, Package: in.Package, MaxDepth: in.MaxDepth, ProjectRoot: in.ProjectRoot}
		_, out, err := h.graphPath(ctx, req, pi)
		return nil, TraceOutput{
			Direction: dir,
			Status:    out.Status,
			Hint:      out.Hint,
			Project:   out.Project,
			Src:       out.Src,
			Dst:       out.Dst,
			Path:      out.Path,
		}, err
	case "impact":
		ii := ImpactInput{Name: in.Symbol, Package: in.Package, MaxDepth: in.MaxDepth, K: in.K, ProjectRoot: in.ProjectRoot}
		_, out, err := h.graphImpact(ctx, req, ii)
		tOut := TraceOutput{
			Direction:  dir,
			Status:     out.Status,
			Hint:       out.Hint,
			Project:    out.Project,
			Targets:    out.Targets,
			Risk:       out.Risk,
			Nodes:      out.Nodes,
			MaxDepth:   out.MaxDepth,
			Total:      out.Total,
			Truncated:  out.Truncated,
			Elided:     out.Elided,
			TestsToRun: out.TestsToRun,
		}
		if out.Status == "ok" && out.Total > 0 && hasNonGoTarget(out.Targets) {
			markImpactPartialRecall(&tOut)
			foldUnresolvedInbound(ctx, h, in.ProjectRoot, &tOut)
		}
		return nil, tOut, err
	default:
		return nil, TraceOutput{Direction: dir, Status: "error", Hint: "direction must be one of: callers, callees, path, impact"}, nil
	}
}

// hasNonGoTarget reports whether any resolved target lives in a non-Go file.
// Non-Go edges are name-based (tree-sitter), so recall is incomplete.
func hasNonGoTarget(targets []TargetMatch) bool {
	for _, t := range targets {
		if t.Path != "" && !strings.HasSuffix(t.Path, ".go") {
			return true
		}
	}
	return false
}

// markImpactPartialRecall tags a non-empty impact blast radius on a non-Go
// (tree-sitter) target as partial-recall. Unlike callers/callees, no grep
// sweep is run: impact is a *transitive* radius and a single bare-symbol grep
// can't reconstruct it, so approximating would over-claim. The honest signal
// is the flag plus a hint pointing at grep for the edges that matter.
func markImpactPartialRecall(out *TraceOutput) {
	out.Recall = "partial"
	partial := fmt.Sprintf("impact: %d node(s) via name-based (tree-sitter) edges, recall partial — the true blast radius may be larger; verify critical edges with grep on the symbol names", out.Total)
	if out.Hint != "" {
		out.Hint += " | " + partial
	} else {
		out.Hint = partial
	}
}

// augmentPartialRecall runs a grep sweep for the bare symbol name and folds
// the results into out.GrepHits (deduped against existing call-site lines).
// Sets out.Recall = "partial" and annotates out.Hint regardless of grep outcome.
func augmentPartialRecall(ctx context.Context, h toolSurface, symbol, projectRoot string, out *TraceOutput) {
	bare := retrieve.BareSymbolName(symbol)
	if bare == "" {
		out.Recall = "partial"
		return
	}
	pattern := `\b` + bare + `\b`
	_, gout, err := h.searchGrep(ctx, nil, SearchGrepInput{
		ProjectRoot: projectRoot,
		Pattern:     pattern,
		MaxResults:  20,
	})

	out.Recall = "partial"

	var grepN int
	if err == nil && (gout.Status == "ok" || gout.Status == "no-matches") {
		// Dedup: skip grep hits already covered by a graph call-site line.
		seenSite := make(map[string]bool, len(out.Hits))
		for _, h := range out.Hits {
			if h.CallSitePath != "" {
				seenSite[fmt.Sprintf("%s:%d", h.CallSitePath, h.CallSiteLine)] = true
			}
		}
		for _, m := range gout.Matches {
			if !seenSite[fmt.Sprintf("%s:%d", m.Path, m.Line)] {
				out.GrepHits = append(out.GrepHits, m)
			}
		}
		grepN = len(out.GrepHits)
	}

	partial := fmt.Sprintf("graph: %d call-edge(s) (name-based, recall partial); grep: %d more candidate site(s)", len(out.Hits), grepN)
	if out.Hint != "" {
		out.Hint += " | " + partial
	} else {
		out.Hint = partial
	}
}

// unresolvedInbounder is the optional capability of a tool surface to attribute
// known-unresolved imports to a file's package (#130). Only the local surfaces
// (*Server and its projectScoped wrapper) implement it; remote/maintenance/test
// surfaces don't, and foldUnresolvedInbound simply skips them — the trace is
// still correct, just without the extra recall signal.
type unresolvedInbounder interface {
	unresolvedInbound(ctx context.Context, projectRoot, file string, limit int) ([]store.UnresolvedInbound, error)
}

// foldUnresolvedInbound queries known-unresolved imports into each non-Go target
// file's package and folds the distinct specifiers (summed by count) into
// out.UnresolvedInbound, plus a hint pointing at grep. Best-effort: skipped when
// the surface can't answer or on any error, and a no-op when there are none, so
// clean traces are byte-identical. Complements the bare-name grep sweep, which
// cannot see these edges because the specifier and the target's name differ.
func foldUnresolvedInbound(ctx context.Context, h toolSurface, projectRoot string, out *TraceOutput) {
	ui, ok := h.(unresolvedInbounder)
	if !ok {
		return
	}
	sum := map[string]int{}
	var order []string
	for _, t := range out.Targets {
		if t.Path == "" || strings.HasSuffix(t.Path, ".go") {
			continue
		}
		rows, err := ui.unresolvedInbound(ctx, projectRoot, t.Path, 0)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if _, seen := sum[r.Specifier]; !seen {
				order = append(order, r.Specifier)
			}
			sum[r.Specifier] += r.Count
		}
	}
	if len(order) == 0 {
		return
	}
	// Stable order: most-frequent first, ties by specifier.
	sort.SliceStable(order, func(i, j int) bool {
		if sum[order[i]] != sum[order[j]] {
			return sum[order[i]] > sum[order[j]]
		}
		return order[i] < order[j]
	})
	total := 0
	for _, spec := range order {
		out.UnresolvedInbound = append(out.UnresolvedInbound, store.UnresolvedInbound{Specifier: spec, Count: sum[spec]})
		total += sum[spec]
	}
	out.Recall = "partial"
	shown := order
	if len(shown) > 3 {
		shown = shown[:3]
	}
	hint := fmt.Sprintf("%d unresolved import(s) into this symbol's package (build-mediated / workspace subpath) that name-based recall cannot see — grep the specifier(s) to confirm: %s",
		total, strings.Join(shown, ", "))
	if out.Hint != "" {
		out.Hint += " | " + hint
	} else {
		out.Hint = hint
	}
}
