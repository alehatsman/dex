package retrieve

import (
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// GraphResult is the call-graph neighborhood emitted alongside an ask
// answer, in neutral (transport-free) form. The transport maps it to
// the wire GraphResult.
type GraphResult struct {
	Nodes []GraphNode
	Edges []GraphEdge
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
// Node IDs and edge from/to are rewritten to a compact form
// (`<pkg-tail>.<qualified-name>`) so agents don't have to parse
// `<module>::<pkg>::<kind>::<qname>` for every reference. The full
// IDs remain available via the in-memory view for any future query
// that takes a graph ID as input.
func EnrichGraph(intent string, view *graphquery.View, semHits []SemHit, symbols []SymHit) (*GraphResult, bool) {
	if view == nil {
		return nil, false
	}
	e := &graphEnricher{
		view:     view,
		semHits:  semHits,
		symbols:  symbols,
		gr:       &GraphResult{Nodes: []GraphNode{}, Edges: []GraphEdge{}},
		seenNode: map[string]bool{},
		seenEdge: map[string]bool{},
	}
	e.runForIntent(intent)
	if len(e.gr.Nodes) == 0 && len(e.gr.Edges) == 0 {
		return nil, false
	}
	return e.gr, true
}

// graphEnricher carries the working state for one EnrichGraph call.
// Hoisting the closures off EnrichGraph into methods keeps the dispatch
// switch short and the helpers individually testable.
type graphEnricher struct {
	view     *graphquery.View
	semHits  []SemHit
	symbols  []SymHit
	gr       *GraphResult
	seenNode map[string]bool
	seenEdge map[string]bool
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
	if e.seenNode[key] || len(e.gr.Nodes) >= MaxGraphNodes {
		return
	}
	e.seenNode[key] = true
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
	if e.seenEdge[key] || len(e.gr.Edges) >= MaxGraphEdges {
		return
	}
	e.seenEdge[key] = true
	e.gr.Edges = append(e.gr.Edges, GraphEdge{
		From: from,
		To:   to,
		Kind: string(ge.Kind),
	})
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

// packageRollup adds package + top-level type/function nodes for every
// package in pkgs.
func (e *graphEnricher) packageRollup(pkgs map[string]struct{}) {
	for pkg := range pkgs {
		for _, n := range e.view.NodesByPackage[pkg] {
			switch n.Kind {
			case graph.NodePackage, graph.NodeType, graph.NodeStruct, graph.NodeInterface, graph.NodeFunction:
				e.addNode(n)
			}
		}
	}
}

// architectureAnchorPkgs caps how many top-PageRank packages seed the
// architecture rollup. Picked to fill MaxGraphNodes/MaxGraphEdges with
// a meaningful cross-section without burning the budget on one package.
const architectureAnchorPkgs = 8

// callsExpansion walks the calls-edges in or out of the matched
// symbols. Direction picks which side the symbol is on:
// `dst` = callers (edges arriving at the symbol), `src` = callees.
func (e *graphEnricher) callsExpansion(direction string) {
	for _, sym := range e.symbols {
		lookup := e.view.NodesByQualified[sym.QualifiedName]
		if len(lookup) == 0 {
			lookup = e.view.NodesByName[sym.QualifiedName]
		}
		for _, n := range lookup {
			e.addNode(n)
			edges := e.view.EdgesBySrc[n.ID]
			if direction == "dst" {
				edges = e.view.EdgesByDst[n.ID]
			}
			for _, ge := range edges {
				if ge.Kind != graph.EdgeCalls {
					continue
				}
				peerID := ge.SrcID
				if direction == "src" {
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

// runForIntent dispatches to the right expansion mix for the intent.
// Default branch (unrecognized intents) unions symbol-neighborhood +
// pkg rollup. behavior_search is explicit: symbol-neighborhood only,
// no pkg rollup (noisy semHits from help-text blobs would otherwise
// dump an entire package's function list as graph noise).
func (e *graphEnricher) runForIntent(intent string) {
	switch intent {
	case IntentSymbolLookup, IntentEditingContext:
		e.symbolNeighborhood()
	case IntentCallers:
		e.callsExpansion("dst")
		if len(e.gr.Nodes) == 0 {
			e.symbolNeighborhood()
		}
	case IntentCallees:
		e.callsExpansion("src")
		if len(e.gr.Nodes) == 0 {
			e.symbolNeighborhood()
		}
	case IntentArchitecture:
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
	case IntentPackageTopology:
		e.packageTopology()
	case IntentBehaviorSearch:
		// For behavioral questions the symbol neighborhood is the right
		// anchor. Package rollup is intentionally omitted: if semantic
		// hits are noisy (e.g. a help-text blob in main.go), rollup
		// dumps the entire main package's function list as graph noise.
		e.symbolNeighborhood()
	default:
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
