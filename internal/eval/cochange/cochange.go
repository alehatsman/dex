// Package cochange measures how much of a repo's blast-radius co-change
// coupling is reachable through the structural (call/import) graph — the
// "structural-coverage ceiling" instrument for issue #555.
//
// Motivation. The blast-radius golden set (eval.GenerateBlastRadius) labels,
// for an anchor file, the OTHER files co-changed with it in the same commit —
// "given I'm editing X, what changes with it?". Retrieval scoring (dex bench
// corpus) shows the src-only subset of these has a persistent rank gap: the
// candidate pool recalls the gold but ranking buries it below k. The only
// lever that could re-rank it without lexical/dense overlap is the graph lane.
// This instrument measures the ceiling on that lever: what fraction of
// src-only blast-radius gold is even *reachable* from its anchor through
// calls/imports edges. Where reachability is low (ripgrep ~0%, despite a
// populated graph) the coupling is genuinely non-structural — co-change
// history reveals it, the call/import graph does not — so no fusion reweight
// can close the gap. The number this pins IS the ceiling that justifies
// parking #555.
//
// GPU-free: like skew/trace it reads only the loaded graph view plus the
// git-mined blast-radius gold; no embedder is involved.
package cochange

import (
	"strings"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// connectingKinds are the edge kinds that link two distinct source files into
// a structural relationship. Calls (a function in file A invokes one in file
// B) are the primary cross-file connector; imports add the module-level edge
// where the importer and imported symbol both resolve to indexed files. Other
// kinds (contains/has_method/…) are intra-file or symbol-to-package and don't
// connect two source files, so they're excluded.
var connectingKinds = []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImports}

// Report is the per-repo structural-coverage measurement over a repo's
// blast-radius golden set.
type Report struct {
	Lang          string `json:"lang"`
	Queries       int    `json:"queries"`         // anchored (blast-radius) queries scored
	TestGold      int    `json:"test_gold"`       // excluded: ≥1 gold file is a test file
	SrcOnly       int    `json:"src_only"`        // measured subset: no gold file is a test file
	AnchorInGraph int    `json:"anchor_in_graph"` // src-only queries whose anchor file has ≥1 graph node
	OneHop        int    `json:"one_hop"`         // src-only with anchor→gold reachable in ≤1 hop
	TwoHop        int    `json:"two_hop"`         // src-only with anchor→gold reachable in ≤2 hops
}

// TestGoldShare is the fraction of blast-radius queries whose gold is
// test-tainted (excluded from the structural measurement). Repo-dependent:
// high for languages with separate test files (Python/Go), ~0 for inline
// tests (Rust) or a separate test module (guava).
func (r Report) TestGoldShare() float64 { return share(r.TestGold, r.Queries) }

// OneHopShare / TwoHopShare are the fraction of src-only queries whose gold is
// reachable from the anchor through calls/imports edges. TwoHopShare is the
// headline ceiling number.
func (r Report) OneHopShare() float64 { return share(r.OneHop, r.SrcOnly) }
func (r Report) TwoHopShare() float64 { return share(r.TwoHop, r.SrcOnly) }

// AnchorResolveShare is the fraction of src-only anchors present in the graph.
// A low value means the reachability numbers undercount (the anchor file has
// no extracted symbols), not that the graph fails to connect — surface it so
// the two are not conflated.
func (r Report) AnchorResolveShare() float64 { return share(r.AnchorInGraph, r.SrcOnly) }

func share(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Compute scores one repo's blast-radius golden set against its loaded graph
// view, using only the structural (calls/imports) connecting kinds. lang
// drives the test-file heuristic. A nil view (no graph indexed) is treated as
// an empty graph: every src-only query is unreachable.
//
// This is the committed #555 baseline — its Report shape and connectingKinds
// input must stay unchanged by #212's co_changes edges (regression safety:
// the new edge kind is additive, never merged into this number). See
// ComputeWithCoChange for the second, additive metric.
func Compute(view *graphquery.View, queries []eval.GoldenQuery, lang string) Report {
	return compute(view, queries, lang, connectingKinds)
}

// ComputeWithCoChange scores the same golden set as Compute, but with
// graph.EdgeCoChanges (#212) added to the connecting kinds — so file pairs
// only linked by git-history temporal coupling, not calls/imports, count as
// reachable too. Reported as a SEPARATE Report from Compute's, never merged:
// it measures how much of the #555 ceiling temporal coupling closes, without
// touching the structural-only baseline other code depends on.
func ComputeWithCoChange(view *graphquery.View, queries []eval.GoldenQuery, lang string) Report {
	kinds := make([]graph.EdgeKind, 0, len(connectingKinds)+1)
	kinds = append(kinds, connectingKinds...)
	kinds = append(kinds, graph.EdgeCoChanges)
	return compute(view, queries, lang, kinds)
}

func compute(view *graphquery.View, queries []eval.GoldenQuery, lang string, kinds []graph.EdgeKind) Report {
	rep := Report{Lang: lang}
	adj := buildFileAdjacency(view, kinds)
	for _, q := range queries {
		if q.Anchor == "" { // not a blast-radius query — skip defensively
			continue
		}
		rep.Queries++
		if anyTestFile(q.RelevantFiles, lang) {
			rep.TestGold++
			continue
		}
		rep.SrcOnly++
		if view != nil {
			if _, ok := view.NodesByPath[q.Anchor]; ok {
				rep.AnchorInGraph++
			}
		}
		if reach(adj, q.Anchor, q.RelevantFiles, 1) {
			rep.OneHop++
			rep.TwoHop++
			continue
		}
		if reach(adj, q.Anchor, q.RelevantFiles, 2) {
			rep.TwoHop++
		}
	}
	return rep
}

// buildFileAdjacency collapses kinds' edges to an undirected file→file
// adjacency. Endpoints whose node has no file path (package/import sentinel
// nodes) and self-edges are dropped, so an entry means "two distinct source
// files are linked by one of kinds".
func buildFileAdjacency(view *graphquery.View, kinds []graph.EdgeKind) map[string]map[string]bool {
	adj := map[string]map[string]bool{}
	if view == nil {
		return adj
	}
	link := func(a, b string) {
		if a == "" || b == "" || a == b {
			return
		}
		if adj[a] == nil {
			adj[a] = map[string]bool{}
		}
		if adj[b] == nil {
			adj[b] = map[string]bool{}
		}
		adj[a][b] = true
		adj[b][a] = true
	}
	for _, kind := range kinds {
		for _, e := range view.EdgesByKind[kind] {
			link(view.NodesByID[e.SrcID].FilePath, view.NodesByID[e.DstID].FilePath)
		}
	}
	return adj
}

// reach reports whether any target file is within maxHop edges of start in the
// undirected file graph (BFS). start is the anchor; targets are the gold files.
func reach(adj map[string]map[string]bool, start string, targets []string, maxHop int) bool {
	want := make(map[string]bool, len(targets))
	for _, t := range targets {
		want[t] = true
	}
	visited := map[string]bool{start: true}
	frontier := []string{start}
	for hop := 0; hop < maxHop && len(frontier) > 0; hop++ {
		var next []string
		for _, f := range frontier {
			for nb := range adj[f] {
				if want[nb] {
					return true
				}
				if !visited[nb] {
					visited[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return false
}

// anyTestFile reports whether any path is a test file under lang's convention.
// A blast-radius query is excluded from the structural measurement when ANY of
// its gold files is a test — a src-excerpt query cannot retrieve a
// token/structure-disjoint test file, so that slice is irreducible noise, not
// a ranking miss (#555 step-2).
func anyTestFile(paths []string, lang string) bool {
	for _, p := range paths {
		if isTestFile(p, lang) {
			return true
		}
	}
	return false
}

func isTestFile(path, lang string) bool {
	p := strings.ToLower(path)
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	switch lang {
	case "go":
		return strings.HasSuffix(base, "_test.go")
	case "python":
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
			base == "conftest.py" || strings.Contains(p, "/tests/") || strings.Contains(p, "/test/")
	case "rust":
		return strings.Contains(p, "/tests/") || strings.Contains(p, "/test/") || strings.HasPrefix(base, "test")
	case "java", "kotlin":
		return strings.HasSuffix(base, "test.java") || strings.HasSuffix(base, "tests.java") ||
			strings.HasSuffix(base, "it.java") || strings.Contains(p, "/test/")
	case "javascript", "typescript", "js", "ts":
		return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
			strings.Contains(p, "/__tests__/") || strings.Contains(p, "/tests/") || strings.Contains(p, "/test/")
	case "c", "cpp", "c++":
		return strings.HasPrefix(base, "test") || strings.HasSuffix(base, "_test.c") ||
			strings.HasSuffix(base, "_test.cc") || strings.HasSuffix(base, "_test.cpp") ||
			strings.Contains(p, "/tests/") || strings.Contains(p, "/test/")
	default:
		return strings.Contains(p, "/tests/") || strings.Contains(p, "/test/") || strings.Contains(base, "test")
	}
}
