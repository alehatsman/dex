package graphquery

import (
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
)

// riskLowMax, riskMediumMax, riskHighMax are the inclusive upper bounds for
// each risk tier. Override via DEX_RISK_LOW_MAX / DEX_RISK_MEDIUM_MAX /
// DEX_RISK_HIGH_MAX env vars at process startup.
var (
	riskLowMax    = envInt("DEX_RISK_LOW_MAX", 1)
	riskMediumMax = envInt("DEX_RISK_MEDIUM_MAX", 4)
	riskHighMax   = envInt("DEX_RISK_HIGH_MAX", 10)
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// RiskLevel classifies a transitive caller count into Low / Medium / High / Critical.
func RiskLevel(n int) string {
	switch {
	case n <= riskLowMax:
		return "Low"
	case n <= riskMediumMax:
		return "Medium"
	case n <= riskHighMax:
		return "High"
	default:
		return "Critical"
	}
}

// TransitiveCallerCount counts the unique callers reachable from seeds up to
// maxDepth hops via EdgeCalls. Same BFS as ComputeImpact but returns only the
// count, avoiding the []Reachable allocation.
func TransitiveCallerCount(view *View, seeds []Node, maxDepth int) int {
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
	count := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, e := range view.EdgesByDst[cur.id] {
			if e.Kind != graph.EdgeCalls || visited[e.SrcID] {
				continue
			}
			visited[e.SrcID] = true
			count++
			queue = append(queue, item{e.SrcID, cur.depth + 1})
		}
	}
	return count
}

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
// Multiple matches are returned so the caller can disambiguate. The optional
// `pkgFilter` collapses ambiguity by package: it matches the full import path
// ("github.com/gotify/server/v2/config") OR any path suffix on a "/" boundary
// ("config", "v2/config") — the same tail convention the "pkg.Foo" symbol form
// uses, so an agent that types the short package name is no longer told the
// symbol doesn't exist (#583).
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
	pkgOK := func(n Node) bool { return pkgFilterMatches(n.PackagePath, pkgFilter) }
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

	// 3) "X.Foo" — split on the last dot (no receiver parens) and match Foo
	//    where X is EITHER the package tail ("pkg.Foo") OR the receiver type
	//    ("Type.Foo" → stored "(*Type).Foo"). The receiver form is what an
	//    agent reaches for first (Indexer.Run); step 1 already covers the
	//    literal "(*Type).Foo" QualifiedName (#571).
	if i := strings.LastIndex(name, "."); i > 0 && !strings.ContainsAny(name, "()*") {
		qualifier, bare := name[:i], name[i+1:]
		for _, n := range view.NodesByName[bare] {
			if pkgTail(n.PackagePath) == qualifier || receiverType(n.QualifiedName) == qualifier {
				add(n)
			}
		}
	}
	return out
}

// pkgFilterMatches reports whether a node's package path satisfies the
// user-supplied `package` filter. An empty filter matches everything. A
// non-empty filter matches the full path, or any path suffix that begins on a
// "/" segment boundary — so "config" and "v2/config" both select
// "github.com/gotify/server/v2/config" while "fig" does not (#583). This is the
// same tail-qualification convention ResolveCallTargets uses for "pkg.Foo".
func pkgFilterMatches(pkgPath, filter string) bool {
	if filter == "" {
		return true
	}
	return pkgPath == filter || strings.HasSuffix(pkgPath, "/"+filter)
}

// PkgFilterCandidates returns the distinct package paths in which `name`
// resolves when no package filter is applied, sorted. A non-empty result means
// the name DOES exist — so when a filtered ResolveCallTargets came back empty,
// the caller can tell the agent "the name is fine, your package filter excluded
// it; here are the packages it lives in" instead of the misleading bare
// not-found that sends the agent mangling the symbol form (#583).
func PkgFilterCandidates(view *View, name string) []string {
	seen := map[string]bool{}
	var pkgs []string
	for _, n := range ResolveCallTargets(view, name, "") {
		if n.PackagePath == "" || seen[n.PackagePath] {
			continue
		}
		seen[n.PackagePath] = true
		pkgs = append(pkgs, n.PackagePath)
	}
	sort.Strings(pkgs)
	return pkgs
}

// pkgTail returns the last path segment of a package path
// ("internal/index" → "index").
func pkgTail(pkgPath string) string {
	if j := strings.LastIndex(pkgPath, "/"); j >= 0 {
		return pkgPath[j+1:]
	}
	return pkgPath
}

// receiverType extracts T from a method QualifiedName like "(*T).Foo" or
// "(T).Foo"; returns "" when qn is not a parenthesized-receiver method.
func receiverType(qn string) string {
	if !strings.HasPrefix(qn, "(") {
		return ""
	}
	closeIdx := strings.Index(qn, ")")
	if closeIdx < 0 {
		return ""
	}
	return strings.TrimPrefix(qn[1:closeIdx], "*")
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
