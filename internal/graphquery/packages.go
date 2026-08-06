package graphquery

import (
	"sort"

	"github.com/alehatsman/dex/internal/graph"
)

// PackageStat is one node in the package import graph: a package (a Go package,
// or a JS/TS per-file module) with its import degrees, PageRank over the import
// DAG, and whether it is a main (executable) package. Fields mirror the mcp
// wire type so callers convert directly.
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

// importTargetPath returns the internal module/package path an import node
// points at, or "" when it has no resolved internal target. Go import nodes
// carry the resolved import path in QualifiedName. Tree-sitter import nodes
// carry the resolved-internal target in Metadata["target"] (set by the
// workspace resolver, #127); an external / unresolved tree-sitter import has no
// target and is dropped — its raw specifier (react, @mui/*) is not a project
// module.
func importTargetPath(n Node) string {
	if n.Language() == "go" {
		return n.QualifiedName
	}
	return n.metaString("target")
}

// BuildPackageGraph derives the internal package import DAG from a loaded View.
// Pure (no I/O) so it unit-tests against a hand-built view. The import graph
// lives on EdgeImports edges: src is the importing package's NodePackage; dst is
// a NodeImport whose resolved target (see importTargetPath) names the imported
// module. An import is "internal" when that target has its own NodePackage in
// the project — external imports (stdlib / third-party / bare npm) have no
// package node and are dropped.
//
// Both Go packages and JS/TS per-file modules participate. Go packages are
// always emitted as nodes (preserving the Go-only behavior for Go repos); a
// tree-sitter module is emitted only when it participates in at least one
// resolved edge, so an isolated fixture file doesn't pad a mostly-Go repo's
// dependency listing.
func BuildPackageGraph(view *View) PackageGraph {
	internal := map[string]struct{}{} // every package path — valid edge endpoints
	goPkg := map[string]struct{}{}    // Go packages — always emitted
	mainByPath := map[string]bool{}   // package clause name == "main" → executable
	for _, n := range view.NodesByID {
		if n.Kind != graph.NodePackage || n.PackagePath == "" {
			continue
		}
		internal[n.PackagePath] = struct{}{}
		if n.Language() == "go" {
			goPkg[n.PackagePath] = struct{}{}
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
		from, to := src.PackagePath, importTargetPath(dst)
		if from == "" || to == "" || from == to {
			continue
		}
		// Both endpoints must be packages in this project. `internal` holds every
		// package path, so this drops external imports (no package node) and any
		// edge whose resolved target isn't an indexed module.
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

	// Emit set: every Go package (preserve Go-repo behavior, including isolated
	// ones) plus any module that participates in a resolved edge. Isolated
	// tree-sitter modules are dropped so they don't pad a mostly-Go repo's
	// dependency listing with orphan fixture files.
	emit := map[string]struct{}{}
	for pkg := range goPkg {
		emit[pkg] = struct{}{}
	}
	for pkg := range inDeg {
		emit[pkg] = struct{}{}
	}
	for pkg := range outDeg {
		emit[pkg] = struct{}{}
	}

	// PageRank over the import DAG. Rank flows importer → imported, so
	// foundation packages many others depend on accumulate weight —
	// the "load-bearing core floats up" signal even when raw in-degree
	// is modest. Keyed by package path (the id space used in outAdj).
	ids := make([]string, 0, len(emit))
	for pkg := range emit {
		ids = append(ids, pkg)
	}
	ranks := graph.PageRank(ids, outAdj)

	nodes := make([]PackageStat, 0, len(emit))
	for pkg := range emit {
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
