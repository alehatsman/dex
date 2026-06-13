package graphquery

import (
	"encoding/json"
	"sort"

	"github.com/alehatsman/dex/internal/graph"
)

// PackageStat is one node in the package import graph: a Go package with its
// import degrees, PageRank over the import DAG, and whether it is a main
// (executable) package. Fields mirror the mcp wire type so callers convert
// directly.
type PackageStat struct {
	Package   string
	InDegree  int
	OutDegree int
	PageRank  float64
	IsMain    bool
}

// PackageImport is one deduped import edge (importer → imported).
type PackageImport struct {
	FromPackage string
	ToPackage   string
}

// PackageGraph is the computed Go package import graph. An empty Nodes slice
// means no Go package graph is indexed (callers map that to "no-graph").
type PackageGraph struct {
	Nodes []PackageStat
	Edges []PackageImport
}

// isGoPackageNode reports whether n is a Go package node. The package import
// DAG is Go-only: the Go extractor emits package nodes with no "language" in
// their metadata, while every tree-sitter extractor stamps its package nodes
// with Metadata["language"] (sitter_javascript.go etc.). So a NodePackage
// without a "language" key is Go; one with it is a non-Go package (web/src TS,
// python/rust/js testdata fixtures) that has no place in this DAG. Nodes carry
// no metadata at all (the common Go case) → Go.
func isGoPackageNode(n Node) bool {
	if n.Kind != graph.NodePackage {
		return false
	}
	if len(n.MetadataJSON) == 0 {
		return true
	}
	var md map[string]any
	if err := json.Unmarshal(n.MetadataJSON, &md); err != nil {
		return true // unparseable metadata: don't exclude a real Go package
	}
	_, nonGo := md["language"]
	return !nonGo
}

// BuildPackageGraph derives the internal package import DAG from a
// loaded View. Pure (no I/O) so it unit-tests against a hand-built
// view. The import graph lives on EdgeImports edges: src is the
// importing package's NodePackage; dst is a NodeImport whose
// QualifiedName is the imported path. An import is "internal" when that
// path has its own NodePackage in the project — external imports
// (stdlib / third-party) have no package node and are dropped.
func BuildPackageGraph(view *View) PackageGraph {
	internal := map[string]struct{}{}
	mainByPath := map[string]bool{} // package clause name == "main" → executable
	for _, n := range view.NodesByID {
		if isGoPackageNode(n) && n.PackagePath != "" {
			internal[n.PackagePath] = struct{}{}
			if n.Name == "main" {
				mainByPath[n.PackagePath] = true
			}
		}
	}
	if len(internal) == 0 {
		return PackageGraph{}
	}

	// Dedup edges on (from, to); build degree counts and the
	// out-adjacency for PageRank in one pass.
	type pair struct{ from, to string }
	seen := map[pair]struct{}{}
	inDeg := map[string]int{}
	outDeg := map[string]int{}
	outAdj := map[string]map[string]struct{}{}
	var edges []PackageImport
	for _, e := range view.EdgesByKind[graph.EdgeImports] {
		src, ok := view.NodesByID[e.SrcID]
		if !ok || src.Kind != graph.NodePackage {
			continue
		}
		dst, ok := view.NodesByID[e.DstID]
		if !ok || dst.Kind != graph.NodeImport {
			continue
		}
		from, to := src.PackagePath, dst.QualifiedName // import node carries the imported path
		if from == "" || to == "" || from == to {
			continue
		}
		// Both endpoints must be Go packages in this project. `internal` is
		// already the Go-package set, so this drops external imports and any
		// edge touching a non-Go (tree-sitter) package node.
		if _, ok := internal[from]; !ok {
			continue
		}
		if _, ok := internal[to]; !ok {
			continue
		}
		if _, dup := seen[pair{from, to}]; dup {
			continue
		}
		seen[pair{from, to}] = struct{}{}
		edges = append(edges, PackageImport{FromPackage: from, ToPackage: to})
		outDeg[from]++
		inDeg[to]++
		if outAdj[from] == nil {
			outAdj[from] = map[string]struct{}{}
		}
		outAdj[from][to] = struct{}{}
	}

	// PageRank over the import DAG. Rank flows importer → imported, so
	// foundation packages many others depend on accumulate weight —
	// the "load-bearing core floats up" signal even when raw in-degree
	// is modest. Keyed by package path (the id space used in outAdj).
	ids := make([]string, 0, len(internal))
	for pkg := range internal {
		ids = append(ids, pkg)
	}
	ranks := graph.PageRank(ids, outAdj)

	nodes := make([]PackageStat, 0, len(internal))
	for pkg := range internal {
		nodes = append(nodes, PackageStat{
			Package:   pkg,
			InDegree:  inDeg[pkg],
			OutDegree: outDeg[pkg],
			PageRank:  ranks[pkg],
			IsMain:    mainByPath[pkg],
		})
	}

	// Deterministic output: nodes by in-degree desc then path; edges by
	// (from, to). Keeps responses and golden tests stable.
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].InDegree != nodes[j].InDegree {
			return nodes[i].InDegree > nodes[j].InDegree
		}
		return nodes[i].Package < nodes[j].Package
	})
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].FromPackage != edges[j].FromPackage {
			return edges[i].FromPackage < edges[j].FromPackage
		}
		return edges[i].ToPackage < edges[j].ToPackage
	})

	return PackageGraph{Nodes: nodes, Edges: edges}
}
