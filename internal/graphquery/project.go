package graphquery

import (
	"sort"

	"github.com/alehatsman/dex/internal/graph"
)

// BuildProjectGraph rolls the per-module import DAG up to workspace projects
// (#127 Phase 3). It is BuildPackageGraph with each endpoint mapped to its owning
// project via projectOf and intra-project edges dropped, so `@bright/ui →
// @bright/common` surfaces once regardless of how many files cross that boundary.
//
// Pure: projectOf is injected (the caller wires resolve.Workspace.ProjectOf), so
// this unit-tests against a hand view with a map-backed mapper — no disk. A
// module owned by no project (projectOf == "") is dropped; that is the
// project-level notion of "internal" (external npm imports already have no target
// and never reach here). PageRank flows importer → imported, so foundation
// projects float up just as in the module graph.
func BuildProjectGraph(view *View, projectOf func(string) string) PackageGraph {
	if view == nil || projectOf == nil {
		return PackageGraph{}
	}

	type pair struct{ from, to string }
	seen := map[pair]struct{}{}
	inDeg := map[string]int{}
	outDeg := map[string]int{}
	outAdj := map[string]map[string]struct{}{}
	emit := map[string]struct{}{}
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
		from := projectOf(src.PackagePath)
		to := projectOf(importTargetPath(dst))
		if from == "" || to == "" || from == to {
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
		emit[from] = struct{}{}
		emit[to] = struct{}{}
	}

	if len(emit) == 0 {
		return PackageGraph{}
	}

	ids := make([]string, 0, len(emit))
	for proj := range emit {
		ids = append(ids, proj)
	}
	ranks := graph.PageRank(ids, outAdj)

	nodes := make([]PackageStat, 0, len(emit))
	for proj := range emit {
		nodes = append(nodes, PackageStat{
			Package:   proj,
			InDegree:  inDeg[proj],
			OutDegree: outDeg[proj],
			PageRank:  ranks[proj],
			// IsMain is a Go executable-package notion; a workspace project has no
			// analogue, so it stays false.
		})
	}

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
