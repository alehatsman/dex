package graphquery

import (
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
)

// Hop is one step in a resolved call/import path. Fields mirror the mcp wire
// type so callers convert directly. EdgeKind names the edge leading into this
// hop ("" for the first hop).
type Hop struct {
	QualifiedName string
	Package       string
	Kind          string
	Path          string
	StartLine     int
	EdgeKind      string
}

// Reachable is one node reached during an impact (reverse-call) walk, with the
// BFS depth at which it was found. Fields mirror the mcp wire type.
type Reachable struct {
	QualifiedName string
	Package       string
	Kind          string
	Path          string
	StartLine     int
	Depth         int
	PageRank      float64
}

// ResolveCallTargets maps the user-supplied `name` (and optional pkg
// filter) onto graph nodes. Recognised shapes, in order:
//
//	"Foo"                  — bare; matches NodeFunction / NodeMethod / NodeType by Name
//	"(*T).Foo" / "T.Foo"   — receiver-qualified; matches by QualifiedName
//	"pkg.Foo"              — package-tail-qualified; PackagePath must end with /pkg or equal pkg
//
// Multiple matches are returned so the caller can disambiguate. The
// optional `pkgFilter` collapses ambiguity by full package path.
func ResolveCallTargets(view *View, name, pkgFilter string) []Node {
	name = strings.TrimSpace(name)
	pkgFilter = strings.TrimSpace(pkgFilter)
	if name == "" {
		return nil
	}
	want := func(n Node) bool {
		switch n.Kind {
		case graph.NodeFunction, graph.NodeMethod:
			return true
		default:
			return false
		}
	}
	pkgOK := func(n Node) bool {
		if pkgFilter == "" {
			return true
		}
		return n.PackagePath == pkgFilter
	}
	out := []Node{}
	seen := map[string]bool{}
	add := func(n Node) {
		if seen[n.ID] || !want(n) || !pkgOK(n) {
			return
		}
		seen[n.ID] = true
		out = append(out, n)
	}

	// 1) Exact QualifiedName match — covers "(*T).Foo", "T.Foo", and
	//    bare function names that happen to be unique within a pkg.
	for _, n := range view.NodesByQualified[name] {
		add(n)
	}
	// 2) Bare Name match — covers "Foo" both as a function name and
	//    as the method portion of "(*T).Foo" (graph stores Name="Foo"
	//    alongside QualifiedName="(*T).Foo").
	for _, n := range view.NodesByName[name] {
		add(n)
	}
	if len(out) > 0 {
		return out
	}

	// 3) "pkg.Foo" — split on the last dot and try pkg-tail matching.
	//    Only attempt when there's exactly one dot and the second
	//    segment looks like an identifier (no receiver parens).
	if i := strings.LastIndex(name, "."); i > 0 && !strings.ContainsAny(name, "()*") {
		pkgTail, bare := name[:i], name[i+1:]
		for _, n := range view.NodesByName[bare] {
			tail := n.PackagePath
			if j := strings.LastIndex(tail, "/"); j >= 0 {
				tail = tail[j+1:]
			}
			if tail == pkgTail {
				add(n)
			}
		}
	}
	return out
}

// BFSPath finds the shortest path from any seed node to any node in dstSet,
// following `calls` and `imports` edges. Returns nil when no path exists
// within maxDepth hops.
func BFSPath(view *View, seeds []Node, dstSet map[string]bool, maxDepth int) []Hop {
	type item struct {
		id       string
		depth    int
		prevID   string
		edgeKind graph.EdgeKind
	}

	visited := map[string]bool{}
	parent := map[string]item{}

	queue := make([]item, 0, len(seeds))
	for _, s := range seeds {
		if dstSet[s.ID] {
			// src == dst: trivial path of one hop
			n := view.NodesByID[s.ID]
			return []Hop{{
				QualifiedName: n.QualifiedName,
				Package:       n.PackagePath,
				Kind:          string(n.Kind),
				Path:          n.FilePath,
				StartLine:     n.StartLine,
			}}
		}
		visited[s.ID] = true
		queue = append(queue, item{id: s.ID, depth: 0})
	}

	var found string
	for len(queue) > 0 && found == "" {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, e := range view.EdgesBySrc[cur.id] {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeImports {
				continue
			}
			if visited[e.DstID] {
				continue
			}
			visited[e.DstID] = true
			parent[e.DstID] = item{id: cur.id, depth: cur.depth, edgeKind: e.Kind}
			if dstSet[e.DstID] {
				found = e.DstID
				break
			}
			queue = append(queue, item{id: e.DstID, depth: cur.depth + 1, prevID: cur.id, edgeKind: e.Kind})
		}
	}

	if found == "" {
		return nil
	}

	// Reconstruct path from found back to seed.
	var ids []string
	cur := found
	for {
		ids = append(ids, cur)
		p, ok := parent[cur]
		if !ok {
			break
		}
		cur = p.id
	}
	// Reverse so it reads src → dst.
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}

	hops := make([]Hop, 0, len(ids))
	for i, id := range ids {
		n, ok := view.NodesByID[id]
		if !ok {
			continue
		}
		hop := Hop{
			QualifiedName: n.QualifiedName,
			Package:       n.PackagePath,
			Kind:          string(n.Kind),
			Path:          n.FilePath,
			StartLine:     n.StartLine,
		}
		if i > 0 {
			hop.EdgeKind = string(parent[id].edgeKind)
		}
		hops = append(hops, hop)
	}
	return hops
}

// ComputeImpact performs a BFS over incoming calls edges (callers
// direction) starting from seeds, up to maxDepth hops. Returns nodes
// sorted by depth asc, PageRank desc, then path+line for determinism.
// Pure over view — unit-testable without a store.
func ComputeImpact(view *View, seeds []Node, maxDepth int) []Reachable {
	type item struct {
		id    string
		depth int
	}
	visited := map[string]bool{}
	for _, t := range seeds {
		visited[t.ID] = true
	}
	queue := make([]item, 0, len(seeds))
	for _, t := range seeds {
		queue = append(queue, item{t.ID, 0})
	}

	var nodes []Reachable
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, e := range view.EdgesByDst[cur.id] {
			if e.Kind != graph.EdgeCalls {
				continue
			}
			if visited[e.SrcID] {
				continue
			}
			visited[e.SrcID] = true
			caller, ok := view.NodesByID[e.SrcID]
			if !ok {
				continue
			}
			nodes = append(nodes, Reachable{
				QualifiedName: caller.QualifiedName,
				Package:       caller.PackagePath,
				Kind:          string(caller.Kind),
				Path:          caller.FilePath,
				StartLine:     caller.StartLine,
				Depth:         cur.depth + 1,
				PageRank:      caller.PageRank,
			})
			queue = append(queue, item{e.SrcID, cur.depth + 1})
		}
	}

	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.PageRank != b.PageRank {
			return a.PageRank > b.PageRank
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.StartLine < b.StartLine
	})
	return nodes
}
