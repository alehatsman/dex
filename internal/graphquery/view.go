// Package graphquery is the query-time graph engine: it loads the indexed
// graph_nodes/graph_edges into an in-memory View and runs the read-only
// algorithms (call/import path BFS, reverse-call impact, Tarjan cycles,
// package import DAG + PageRank, doc-link/tag traversals) that the MCP graph
// tools serve. It depends only on the store and graph schema, never on the
// transport — results are returned as neutral types the transport maps to
// its wire structs.
package graphquery

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/store"
)

// View holds an in-memory snapshot of graph_nodes/graph_edges
// indexed for the queries the router needs. All maps point into the
// same underlying node/edge slices so memory cost is one slice copy.
type View struct {
	NodesByID        map[string]Node
	NodesByName      map[string][]Node // bare name → matching nodes
	NodesByQualified map[string][]Node // qualified name → matching nodes
	NodesByPackage   map[string][]Node // package path → all nodes in pkg
	NodesByPath      map[string][]Node // file path → all nodes in file
	EdgesBySrc       map[string][]Edge
	EdgesByDst       map[string][]Edge
	EdgesByKind      map[graph.EdgeKind][]Edge
}

type Node struct {
	ID            string
	Kind          graph.NodeKind
	Name          string
	QualifiedName string
	PackagePath   string
	FilePath      string
	StartLine     int
	EndLine       int
	// Centrality columns, populated from graph_nodes. Used by call-edge
	// tools to sort peers by importance and to compose the role hint
	// attached to each result.
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	PageRank        float64
	Betweenness     float64
	CommunityID     int
	// MetadataJSON is the raw graph_nodes.metadata_json payload, kept
	// unparsed so the hot paths pay nothing. Accessors Language() and
	// metaString() parse it lazily — e.g. the package-graph tool reads the
	// "language" key and the resolver's import "target" annotation from it.
	MetadataJSON []byte
}

// Language reports the node's source language using the convention every
// extractor follows: the tree-sitter extractors stamp Metadata["language"]
// (see the sitter_*.go files), while the Go extractor leaves it absent. So a
// node with no metadata, unparseable metadata, or no "language" key is Go.
//
// Callers use this to distinguish Go's type-resolved call edges from the
// name-based (tree-sitter) edges, whose recall is incomplete — an empty
// call-graph result on a name-based language is not proof that no edges exist.
func (n Node) Language() string {
	if len(n.MetadataJSON) == 0 {
		return "go"
	}
	var md map[string]any
	if err := json.Unmarshal(n.MetadataJSON, &md); err != nil {
		return "go"
	}
	if lang, ok := md["language"].(string); ok && lang != "" {
		return lang
	}
	return "go"
}

// metaString returns the string value of a metadata key, or "" if the metadata
// is absent, unparseable, or the key is missing / not a string. Parallels
// Language()'s tolerant parse; used to read the workspace resolver's
// import-target annotation (#127).
func (n Node) metaString(key string) string {
	if len(n.MetadataJSON) == 0 {
		return ""
	}
	var md map[string]any
	if err := json.Unmarshal(n.MetadataJSON, &md); err != nil {
		return ""
	}
	s, _ := md[key].(string)
	return s
}

type Edge struct {
	Kind      graph.EdgeKind
	SrcID     string
	DstID     string
	FilePath  string
	StartLine int
}

// DocNodes returns every markdown document node in the view. Used by the
// doc-graph tools to count/scan documents for basename resolution and to
// tell "no doc graph indexed" from "unknown doc path". Order is
// unspecified (map iteration); callers that need determinism sort.
func (v *View) DocNodes() []Node {
	var out []Node
	for _, n := range v.NodesByID {
		if n.Kind == graph.NodeDocument {
			out = append(out, n)
		}
	}
	return out
}

// Load pulls every node and edge from the store and indexes
// them. Returns nil (no error) when the project has no graph indexed
// — the caller should treat that as "graph not available."
func Load(ctx context.Context, st *store.Store) (*View, error) {
	nodes, edges, err := st.GraphStats(ctx)
	if err != nil {
		return nil, err
	}
	if nodes == 0 && edges == 0 {
		return nil, nil
	}

	nodeRows, err := st.GraphAllNodes(ctx)
	if err != nil {
		return nil, err
	}
	edgeRows, err := st.GraphAllEdges(ctx)
	if err != nil {
		return nil, err
	}

	v := &View{
		NodesByID:        make(map[string]Node, len(nodeRows)),
		NodesByName:      map[string][]Node{},
		NodesByQualified: map[string][]Node{},
		NodesByPackage:   map[string][]Node{},
		NodesByPath:      map[string][]Node{},
		EdgesBySrc:       map[string][]Edge{},
		EdgesByDst:       map[string][]Edge{},
		EdgesByKind:      map[graph.EdgeKind][]Edge{},
	}
	for _, r := range nodeRows {
		n := Node{
			ID:              r.ID,
			Kind:            graph.NodeKind(r.Kind),
			Name:            r.Name,
			QualifiedName:   r.QualifiedName,
			PackagePath:     r.PackagePath,
			FilePath:        r.FilePath,
			StartLine:       r.StartLine,
			EndLine:         r.EndLine,
			InDegree:        r.InDegree,
			OutDegree:       r.OutDegree,
			CrossPkgCallers: r.CrossPkgCallers,
			PageRank:        r.PageRank,
			Betweenness:     r.Betweenness,
			CommunityID:     r.CommunityID,
			MetadataJSON:    r.MetadataJSON,
		}
		v.NodesByID[n.ID] = n
		if n.Name != "" {
			v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		}
		if n.QualifiedName != "" && n.QualifiedName != n.Name {
			v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
		}
		if n.PackagePath != "" {
			v.NodesByPackage[n.PackagePath] = append(v.NodesByPackage[n.PackagePath], n)
		}
		if n.FilePath != "" {
			v.NodesByPath[n.FilePath] = append(v.NodesByPath[n.FilePath], n)
		}
	}
	for _, r := range edgeRows {
		e := Edge{
			Kind:      graph.EdgeKind(r.Kind),
			SrcID:     r.SrcID,
			DstID:     r.DstID,
			FilePath:  r.FilePath,
			StartLine: r.StartLine,
		}
		v.EdgesBySrc[e.SrcID] = append(v.EdgesBySrc[e.SrcID], e)
		v.EdgesByDst[e.DstID] = append(v.EdgesByDst[e.DstID], e)
		v.EdgesByKind[e.Kind] = append(v.EdgesByKind[e.Kind], e)
	}
	return v, nil
}

// ChunkPageRank resolves a chunk's PageRank via the in-memory graph
// view. Used by pickSuggestedReads as a tiebreaker for exploration
// intents (architecture / package_topology) so a high-centrality hub
// like Indexer.Run beats a marginally-higher-scored tuning doc when
// scores cluster.
//
// Resolution prefers the node whose declared line range covers
// startLine; falls back to the highest-PageRank node in the file when
// none matches (chunks anchored at line 0 — file-level entries — fall
// back to the file's most-central symbol). Returns 0
// when no graph node exists for the path — non-Go files, top-level
// consts, no graph indexed — which makes the tiebreaker degrade
// silently to "no preference."
func ChunkPageRank(view *View, path string, startLine int) float64 {
	if view == nil {
		return 0
	}
	nodes := view.NodesByPath[path]
	if len(nodes) == 0 {
		return 0
	}
	var bestCovering float64
	for _, n := range nodes {
		if startLine >= n.StartLine && startLine <= n.EndLine && n.PageRank > bestCovering {
			bestCovering = n.PageRank
		}
	}
	if bestCovering > 0 {
		return bestCovering
	}
	var bestAny float64
	for _, n := range nodes {
		if n.PageRank > bestAny {
			bestAny = n.PageRank
		}
	}
	return bestAny
}

// TopPackagesByPageRank returns the K packages with the highest
// aggregate PageRank (sum across all nodes in the package). Used by
// architecture rollup to seed the graph with the project's central
// packages instead of depending on whatever semHits happened to surface
// — a docs-dominated semantic lane otherwise collapses the rollup to
// the single Go file that leaked in. Packages with zero aggregate
// PageRank are skipped: missing centrality data means the graph rerank
// pass hasn't run and seeding would be arbitrary.
func (v *View) TopPackagesByPageRank(k int) map[string]struct{} {
	type pkgScore struct {
		pkg string
		pr  float64
	}
	scores := make([]pkgScore, 0, len(v.NodesByPackage))
	for pkg, nodes := range v.NodesByPackage {
		var sum float64
		for _, n := range nodes {
			sum += n.PageRank
		}
		if sum <= 0 {
			continue
		}
		scores = append(scores, pkgScore{pkg, sum})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].pr > scores[j].pr })
	if len(scores) > k {
		scores = scores[:k]
	}
	out := make(map[string]struct{}, len(scores))
	for _, s := range scores {
		out[s.pkg] = struct{}{}
	}
	return out
}
