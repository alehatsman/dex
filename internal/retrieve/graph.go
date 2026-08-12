package retrieve

import (
	"slices"
	"strings"
	"unicode"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// callDirection specifies which side of a calls edge the symbol occupies.
type callDirection bool

const (
	callsInbound  callDirection = true  // symbol is destination → walk inbound edges to find callers
	callsOutbound callDirection = false // symbol is source → walk outbound edges to find callees
)

// GraphResult is the call-graph neighborhood emitted alongside an ask
// answer, in neutral (transport-free) form. The transport maps it to
// the wire GraphResult.
type GraphResult struct {
	Nodes []GraphNode
	Edges []GraphEdge

	// Resolved / RecallPartial summarise the surfaced call edges for the
	// Trust envelope (#95c). Domain-only — the transport maps just
	// Nodes+Edges to the wire GraphResult; these feed pack.Trust instead.
	Resolved      bool // all surfaced call edges are Go (type-resolved)
	RecallPartial bool // a surfaced call edge is name-based → recall incomplete
}

type GraphNode struct {
	ID            string
	QualifiedName string
	Kind          string
}

type GraphEdge struct {
	From string
	To   string
	Kind string
}

// MaxGraphNodes / MaxGraphEdges bound the graph lane so a big package
// (e.g. mooncake's cmd/* with dozens of entries) can't blow the
// response budget by itself. Hit empirically: 30/50 keeps the lane
// useful for orientation without dominating the payload, and survives
// `no_inline: true` so callers can rely on bundle size being roughly
// proportional to k. Truncation is silent — once full, further nodes
// and edges are dropped.
const (
	MaxGraphNodes = 30
	MaxGraphEdges = 50
)

// EnrichGraph builds the call-graph neighborhood for the resolved
// intent. Returns the result and whether anything was emitted — the
// caller uses the bool to keep avoid/next_action consistent.
//
// What each intent gets (relocated from the former mcp graph wrapper, #114):
//
//	symbol_lookup     — neighbors of the matched symbol (sibling methods,
//	                    fields, embedded types) so the agent sees a type's
//	                    whole "shape" without reading the file.
//	editing_context   — same neighborhood, plus the enclosing type so
//	                    refactors know what else uses the type.
//	architecture      — package/type roll-up for packages surfaced by the
//	                    semantic lane, anchored on PageRank.
//	package_topology  — the workspace-project import DAG when the repo is a
//	                    JS/TS monorepo (projectOf resolves projects), else the
//	                    module-level import edges between packages in the
//	                    neighborhood. See projectTopology / packageTopology (#151).
//	callers           — incoming calls edges into matched symbols (Go-only;
//	                    other languages fall back to BM25 chunk search).
//	callees           — outgoing calls edges from matched symbols.
//
// projectOf maps an internal module/package path to its owning workspace
// project (resolve.Workspace.ProjectOf), injected by the caller so this layer
// stays transport-free; nil (no root / not a workspace) disables the project
// rollup and package_topology falls back to the module lane.
//
// Node IDs and edge from/to are rewritten to a compact form
// (`<pkg-tail>.<qualified-name>`) so agents don't have to parse
// `<module>::<pkg>::<kind>::<qname>` for every reference. The full
// IDs remain available via the in-memory view for any future query
// that takes a graph ID as input.
func EnrichGraph(intent string, view *graphquery.View, semHits []SemHit, symbols []SymbolHit, projectOf func(string) string) (*GraphResult, bool) {
	if view == nil {
		return nil, false
	}
	e := &graphEnricher{
		view:      view,
		semHits:   semHits,
		symbols:   symbols,
		projectOf: projectOf,
		gr:        &GraphResult{Nodes: []GraphNode{}, Edges: []GraphEdge{}},
		seenNode:  map[string]struct{}{},
		seenEdge:  map[string]struct{}{},
	}
	e.runForIntent(intent)
	if len(e.gr.Nodes) == 0 && len(e.gr.Edges) == 0 {
		return nil, false
	}
	// Summarise call-edge resolution for the Trust envelope (#95c): a
	// neighborhood is "resolved" only if it surfaced call edges and none
	// were name-based (tree-sitter, non-Go).
	e.gr.Resolved = e.sawCallEdge && !e.nameBasedCall
	e.gr.RecallPartial = e.nameBasedCall
	return e.gr, true
}

// graphEnricher carries the working state for one EnrichGraph call.
// Hoisting the closures off EnrichGraph into methods keeps the dispatch
// switch short and the helpers individually testable.
type graphEnricher struct {
	view      *graphquery.View
	semHits   []SemHit
	symbols   []SymbolHit
	projectOf func(string) string // #151: workspace-project mapper, nil when N/A
	gr        *GraphResult
	seenNode  map[string]struct{}
	seenEdge  map[string]struct{}
	// Trust-envelope tallies (#95c), accumulated as call edges are surfaced.
	sawCallEdge   bool // any EdgeCalls surfaced → there is a resolved claim to judge
	nameBasedCall bool // a surfaced call edge touches a non-Go (tree-sitter) node
}

func (e *graphEnricher) addNode(n graphquery.Node) {
	// Import nodes are emitted per-file in layer 1, so the same
	// dependency (e.g. `fmt`) shows up as N distinct node IDs.
	// Dedup on QualifiedName for imports so the agent sees one
	// entry per dependency, not one per importing file.
	key := n.ID
	if n.Kind == graph.NodeImport && n.QualifiedName != "" {
		key = "import:" + n.QualifiedName
	}
	if _, ok := e.seenNode[key]; ok || len(e.gr.Nodes) >= MaxGraphNodes {
		return
	}
	e.seenNode[key] = struct{}{}
	// For imports the compactID already encodes the import path, so
	// QualifiedName is redundant — drop it on the wire.
	qname := n.QualifiedName
	if n.Kind == graph.NodeImport {
		qname = ""
	}
	e.gr.Nodes = append(e.gr.Nodes, GraphNode{
		ID:            CompactID(n),
		QualifiedName: qname,
		Kind:          string(n.Kind),
	})
}

func (e *graphEnricher) addEdge(ge graphquery.Edge) {
	from, to := ge.SrcID, ge.DstID
	if n, ok := e.view.NodesByID[ge.SrcID]; ok {
		from = CompactID(n)
	}
	if n, ok := e.view.NodesByID[ge.DstID]; ok {
		to = CompactID(n)
	}
	// Dedup on the compact (from,kind,to) triple — the raw IDs
	// can differ for per-file import nodes that collapse to the
	// same dependency on the wire (see addNode), so a raw-ID
	// dedup leaks duplicates like `src -> fmt` × N.
	key := from + "|" + string(ge.Kind) + "|" + to
	if _, ok := e.seenEdge[key]; ok || len(e.gr.Edges) >= MaxGraphEdges {
		return
	}
	e.seenEdge[key] = struct{}{}
	e.gr.Edges = append(e.gr.Edges, GraphEdge{
		From: from,
		To:   to,
		Kind: string(ge.Kind),
	})
	// Trust envelope (#95c): a surfaced call edge is type-resolved only when
	// both endpoints are Go nodes; anything else is name-based (tree-sitter)
	// with incomplete recall.
	if ge.Kind == graph.EdgeCalls {
		e.sawCallEdge = true
		if !isGoNode(e.view, ge.SrcID) || !isGoNode(e.view, ge.DstID) {
			e.nameBasedCall = true
		}
	}
}

// isGoNode reports whether id resolves to a Go node in the view. A missing or
// non-Go endpoint is treated as name-based — an unresolved edge is not a
// type-resolved claim.
func isGoNode(view *graphquery.View, id string) bool {
	n, ok := view.NodesByID[id]
	return ok && n.Language() == "go"
}

// symbolNeighborhood surfaces each matched symbol's container (parent
// type for methods/fields) and its siblings (other methods/fields on
// the same type, embedded types).
func (e *graphEnricher) symbolNeighborhood() {
	for _, sym := range e.symbols {
		lookup := e.view.NodesByName[sym.QualifiedName]
		if len(lookup) == 0 {
			// Some MCP symbol hits use the bare method name even
			// when the graph stored a qualified form like (*T).M.
			lookup = e.view.NodesByQualified[sym.QualifiedName]
		}
		for _, n := range lookup {
			e.addNode(n)
			for _, parentEdge := range e.view.EdgesByDst[n.ID] {
				if parentEdge.Kind != graph.EdgeHasMethod && parentEdge.Kind != graph.EdgeHasField {
					continue
				}
				parent, ok := e.view.NodesByID[parentEdge.SrcID]
				if !ok {
					continue
				}
				e.addNode(parent)
				e.addEdge(parentEdge)
				for _, sibling := range e.view.EdgesBySrc[parent.ID] {
					if sibling.Kind != graph.EdgeHasMethod && sibling.Kind != graph.EdgeHasField && sibling.Kind != graph.EdgeEmbeds {
						continue
					}
					e.addEdge(sibling)
					if dst, ok := e.view.NodesByID[sibling.DstID]; ok {
						e.addNode(dst)
					}
				}
			}
		}
	}
}

// packageRollup adds, for every package in pkgs, the package node plus its
// most structurally central type/function nodes.
//
// Two passes keep the rollup a balanced cross-section rather than letting the
// first-iterated package monopolize the node budget (#537): pass 1 anchors
// every package node so no anchor is starved, pass 2 fills each package's
// members under a per-package cap, ranked by PageRank so central symbols win
// the budget instead of alphabetically-first ones. Iteration order is sorted
// for determinism (map ranging is random).
func (e *graphEnricher) packageRollup(pkgs map[string]struct{}) {
	if len(pkgs) == 0 {
		return
	}
	ordered := make([]string, 0, len(pkgs))
	for pkg := range pkgs {
		ordered = append(ordered, pkg)
	}
	slices.Sort(ordered)

	// Pass 1: anchor every package node first.
	for _, pkg := range ordered {
		for _, n := range e.view.NodesByPackage[pkg] {
			if n.Kind == graph.NodePackage {
				e.addNode(n)
			}
		}
	}

	// Pass 2: fill with each package's most central members.
	perPkg := MaxGraphNodes / len(pkgs)
	if perPkg < 1 {
		perPkg = 1
	}
	for _, pkg := range ordered {
		e.rollupMembers(pkg, perPkg)
	}
}

// rollupNoise are low-signal trailing identifiers that make poor architecture
// representatives — common unexported helpers/error vars that float up by
// PageRank but say nothing about a package's shape (#570). All lowercase, so
// they only ever match unexported symbols. ≤2-char names are dropped by length.
var rollupNoise = map[string]struct{}{
	"err": {}, "ctx": {}, "tmp": {}, "val": {}, "ret": {},
	"res": {}, "out": {}, "cur": {}, "idx": {}, "cnt": {},
	"buf": {}, "msg": {}, "req": {}, "obj": {},
}

// trailingIdent returns the final identifier of a qualified name, e.g.
// "(*Server).Run" → "Run", "mcp.writeJSON" → "writeJSON", "DurMS" → "DurMS".
func trailingIdent(qn string) string {
	if i := strings.LastIndexAny(qn, "."); i >= 0 && i < len(qn)-1 {
		return qn[i+1:]
	}
	return qn
}

// rollupMembers adds up to limit of pkg's type/function nodes as the package's
// architecture representatives. Exported symbols rank first — they are the
// package's API surface, which is what an architecture/topology view is about;
// unexported helpers (writeJSON, joinSlice, err) are implementation detail and
// only fill remaining slots when a package exposes fewer than limit exported
// symbols (so helper-only packages like main are still represented). Trivial
// noise names are dropped outright. PageRank then QualifiedName order within
// each exported/unexported tier keeps the choice deterministic (#570).
func (e *graphEnricher) rollupMembers(pkg string, limit int) {
	var members []graphquery.Node
	for _, n := range e.view.NodesByPackage[pkg] {
		switch n.Kind {
		case graph.NodeType, graph.NodeStruct, graph.NodeInterface, graph.NodeFunction:
			name := trailingIdent(n.QualifiedName)
			_, inNoise := rollupNoise[strings.ToLower(name)]
			if len(name) <= 2 || inNoise {
				continue
			}
			members = append(members, n)
		}
	}
	exported := func(n graphquery.Node) bool {
		for _, r := range trailingIdent(n.QualifiedName) {
			return unicode.IsUpper(r)
		}
		return false
	}
	slices.SortStableFunc(members, func(a, b graphquery.Node) int {
		if ea, eb := exported(a), exported(b); ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		if a.PageRank != b.PageRank {
			if a.PageRank > b.PageRank {
				return -1
			}
			return 1
		}
		return strings.Compare(a.QualifiedName, b.QualifiedName)
	})
	if len(members) > limit {
		members = members[:limit]
	}
	for _, n := range members {
		e.addNode(n)
	}
}

// importEdgesAmong emits internal `imports` edges whose importing package is
// in pkgs, so a package rollup carries real inter-package structure instead
// of a flat, edgeless node list (#537). External (stdlib/third-party) imports
// are skipped — only edges to a path that has its own package node in the
// project are kept, keeping the topology about the project itself.
func (e *graphEnricher) importEdgesAmong(pkgs map[string]struct{}) {
	for _, ge := range e.view.EdgesByKind[graph.EdgeImports] {
		src, ok := e.view.NodesByID[ge.SrcID]
		if !ok || src.Kind != graph.NodePackage {
			continue
		}
		if _, in := pkgs[src.PackagePath]; !in {
			continue
		}
		dst, ok := e.view.NodesByID[ge.DstID]
		if !ok || dst.Kind != graph.NodeImport {
			continue
		}
		if len(e.view.NodesByPackage[dst.QualifiedName]) == 0 {
			continue // external import — not part of the project topology
		}
		e.addNode(src)
		e.addNode(dst)
		e.addEdge(ge)
	}
}

// architectureAnchorPkgs caps how many top-PageRank packages seed the
// architecture rollup. Picked to fill MaxGraphNodes/MaxGraphEdges with
// a meaningful cross-section without burning the budget on one package.
const architectureAnchorPkgs = 8

// callsExpansion walks the calls-edges in or out of the matched symbols.
// callsInbound finds callers (symbol is edge destination); callsOutbound finds callees (symbol is edge source).
func (e *graphEnricher) callsExpansion(direction callDirection) {
	for _, sym := range e.symbols {
		lookup := e.view.NodesByQualified[sym.QualifiedName]
		if len(lookup) == 0 {
			lookup = e.view.NodesByName[sym.QualifiedName]
		}
		for _, n := range lookup {
			e.addNode(n)
			edges := e.view.EdgesBySrc[n.ID]
			if direction == callsInbound {
				edges = e.view.EdgesByDst[n.ID]
			}
			for _, ge := range edges {
				if ge.Kind != graph.EdgeCalls {
					continue
				}
				peerID := ge.SrcID
				if direction == callsOutbound {
					peerID = ge.DstID
				}
				if peer, ok := e.view.NodesByID[peerID]; ok {
					e.addNode(peer)
					e.addEdge(ge)
				}
			}
		}
	}
}

// packageTopology surfaces imports between packages in the semantic
// neighborhood. Always seeds package nodes themselves so the topology
// has anchors even when no import edges resolve.
func (e *graphEnricher) packageTopology() {
	pkgs := packagesFromPaths(e.view, e.semHits)
	for pkg := range pkgs {
		for _, n := range e.view.NodesByPackage[pkg] {
			if n.Kind == graph.NodePackage {
				e.addNode(n)
			}
		}
	}
	for _, ge := range e.view.EdgesByKind[graph.EdgeImports] {
		srcN, srcOK := e.view.NodesByID[ge.SrcID]
		if !srcOK {
			continue
		}
		if _, in := pkgs[srcN.PackagePath]; !in {
			continue
		}
		e.addNode(srcN)
		if dst, ok := e.view.NodesByID[ge.DstID]; ok {
			e.addNode(dst)
		}
		e.addEdge(ge)
	}
}

// projectTopology surfaces the workspace-project import DAG for a JS/TS
// monorepo (#151): it rolls the per-file module import graph up to workspace
// projects via graphquery.BuildProjectGraph and emits one node per project +
// the deduped cross-project import edges. The project names are already the
// compact IDs, so the nodes/edges go straight onto the wire with no view
// lookup. Returns false (emitting nothing) when there is no project mapper (the
// caller only supplies one for a genuine workspace root, so this is the Go path)
// or the workspace has no cross-project edges — the package_topology dispatch
// then falls back to the neighborhood module topology.
//
// Node order from BuildProjectGraph is in-degree descending, so when the
// MaxGraphNodes/MaxGraphEdges caps bite, the most load-bearing foundation
// projects are what survive.
func (e *graphEnricher) projectTopology() bool {
	if e.projectOf == nil {
		return false
	}
	pg := graphquery.BuildProjectGraph(e.view, e.projectOf)
	if len(pg.Nodes) == 0 {
		return false
	}
	for _, n := range pg.Nodes {
		if _, ok := e.seenNode[n.Package]; ok || len(e.gr.Nodes) >= MaxGraphNodes {
			continue
		}
		e.seenNode[n.Package] = struct{}{}
		e.gr.Nodes = append(e.gr.Nodes, GraphNode{ID: n.Package, Kind: string(graph.NodePackage)})
	}
	for _, ed := range pg.Edges {
		key := ed.FromPackage + "|" + string(graph.EdgeImports) + "|" + ed.ToPackage
		if _, ok := e.seenEdge[key]; ok || len(e.gr.Edges) >= MaxGraphEdges {
			continue
		}
		e.seenEdge[key] = struct{}{}
		e.gr.Edges = append(e.gr.Edges, GraphEdge{From: ed.FromPackage, To: ed.ToPackage, Kind: string(graph.EdgeImports)})
	}
	return len(e.gr.Nodes) > 0
}

// runForIntent dispatches to the right expansion mix for the intent.
// Default branch (unrecognized intents) unions symbol-neighborhood +
// pkg rollup. behavior_search is explicit: symbol-neighborhood only,
// no pkg rollup (noisy semHits from help-text blobs would otherwise
// dump an entire package's function list as graph noise).
func (e *graphEnricher) runForIntent(intent string) {
	// Which graph lane an intent anchors on is decided by the #95d evidence
	// policy (policy.go); the lane implementations stay here.
	switch PolicyFor(intent).GraphLane {
	case GraphLaneNeighborhood:
		// symbol_lookup / editing_context / behavior_search: symbol
		// neighborhood only. Package rollup is intentionally omitted — if
		// semantic hits are noisy (e.g. a help-text blob in main.go), rollup
		// dumps the entire main package's function list as graph noise.
		e.symbolNeighborhood()
	case GraphLaneCallersInbound:
		e.callsExpansion(callsInbound)
		if len(e.gr.Nodes) == 0 {
			e.symbolNeighborhood()
		}
	case GraphLaneCalleesOutbound:
		e.callsExpansion(callsOutbound)
		if len(e.gr.Nodes) == 0 {
			e.symbolNeighborhood()
		}
	case GraphLaneArchitecture:
		// Anchor on the project's structurally central packages so the
		// rollup stays useful even when the semantic lane skews to docs
		// and surfaces only one Go file by accident. PageRank-derived
		// anchors first; semHit-derived packages augment so a question
		// that does point at a specific subsystem still pulls that
		// subsystem in.
		pkgs := e.view.TopPackagesByPageRank(architectureAnchorPkgs)
		for pkg := range packagesFromPaths(e.view, e.semHits) {
			pkgs[pkg] = struct{}{}
		}
		e.packageRollup(pkgs)
		// Emit the inter-package import edges so the architecture view shows
		// real structure, not a flat node list — otherwise the `avoid` hint
		// ("these nodes ARE the structural overview") would be a lie (#537).
		e.importEdgesAmong(pkgs)
	case GraphLanePackageTopology:
		// Project rollup first (#151): when projectOf is set — the caller resolved
		// a genuine workspace root (resolve.IsWorkspaceRoot) — the whole-workspace
		// project DAG is the right answer for a JS/TS monorepo, and is stable
		// regardless of which files the semantic lane happened to surface. It is
		// nil for a Go repo (no workspace root), so projectTopology emits nothing
		// and we fall back to the neighborhood module topology, which is correct
		// for Go. The root gate — not the emptiness of the rollup — is what keeps a
		// Go repo's buried JS/TS test fixtures from ever surfacing here.
		if !e.projectTopology() {
			e.packageTopology()
		}
	default: // GraphLaneNeighborhoodRollup — assemble + unrecognized intents
		e.symbolNeighborhood()
		e.packageRollup(packagesFromPaths(e.view, e.semHits))
	}
}

// packagesFromPaths collects the set of package paths that contain at
// least one of the file paths in semHits. Lets architecture /
// package_topology focus on the neighborhood the user is actually
// asking about, instead of dumping the whole graph.
func packagesFromPaths(view *graphquery.View, semHits []SemHit) map[string]struct{} {
	pkgs := map[string]struct{}{}
	for _, h := range semHits {
		for _, n := range view.NodesByPath[h.Path] {
			if n.PackagePath != "" {
				pkgs[n.PackagePath] = struct{}{}
			}
		}
	}
	return pkgs
}

// CompactID condenses internal/graph.NodeID's
// `<module>::<pkg>::<kind>::<qualified-name>` into a form an agent can
// scan at a glance. Kept stable within one response so edges and
// nodes refer to the same string. Examples:
//
//	mcp.(*Server).ContextRouter    — methods, functions, types, fields
//	internal/mcp                    — packages (qualified_name *is* the path)
//	github.com/foo/bar              — imports (qualified_name is the path)
func CompactID(n graphquery.Node) string {
	switch n.Kind {
	case graph.NodePackage:
		if n.PackagePath != "" {
			return PkgTail(n.PackagePath)
		}
		return n.QualifiedName
	case graph.NodeImport:
		return n.QualifiedName
	}
	tail := PkgTail(n.PackagePath)
	if n.QualifiedName != "" {
		if tail != "" {
			return tail + "." + n.QualifiedName
		}
		return n.QualifiedName
	}
	if tail != "" && n.Name != "" {
		return tail + "." + n.Name
	}
	return n.Name
}

// PkgTail returns the last path segment of pkg, e.g.
// "github.com/x/y/internal/mcp" → "mcp". Empty string in, empty out.
func PkgTail(pkg string) string {
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		return pkg[i+1:]
	}
	return pkg
}
