package graph

import (
	"context"
	"math"
	"testing"
)

// TestComputeCentrality builds a tiny call graph by hand and verifies
// the degree / cross-pkg / PageRank outputs. Keeps the math test
// decoupled from go/types extraction.
func TestComputeCentrality(t *testing.T) {
	// Three nodes across two packages:
	//   a (pkg P) calls b (pkg P)
	//   a (pkg P) calls c (pkg Q)
	//   d (pkg Q) calls c (pkg Q)
	//   e (pkg Q) calls c (pkg Q)
	//
	// Expectations:
	//   c.InDegree         = 3 (callers a, d, e)
	//   c.CrossPkgCallers  = 1 (pkg P, not Q)
	//   b.InDegree         = 1, b.CrossPkgCallers = 0 (same-pkg only)
	//   a.OutDegree        = 2 (b, c)
	nodes := []Node{
		{ID: "a", Kind: NodeFunction, PackagePath: "P"},
		{ID: "b", Kind: NodeFunction, PackagePath: "P"},
		{ID: "c", Kind: NodeFunction, PackagePath: "Q"},
		{ID: "d", Kind: NodeFunction, PackagePath: "Q"},
		{ID: "e", Kind: NodeFunction, PackagePath: "Q"},
	}
	edges := []Edge{
		{Kind: EdgeCalls, SrcID: "a", DstID: "b"},
		{Kind: EdgeCalls, SrcID: "a", DstID: "c"},
		{Kind: EdgeCalls, SrcID: "d", DstID: "c"},
		{Kind: EdgeCalls, SrcID: "e", DstID: "c"},
		// Repeated call-site between the same pair — must NOT inflate
		// in_degree (distinct (src,dst) only).
		{Kind: EdgeCalls, SrcID: "a", DstID: "c", StartLine: 99},
		// Non-calls edge — must be ignored entirely.
		{Kind: EdgeHasMethod, SrcID: "a", DstID: "b"},
	}

	got := ComputeCentrality(nodes, edges)

	if got["c"].InDegree != 3 {
		t.Errorf("c.InDegree = %d, want 3", got["c"].InDegree)
	}
	if got["c"].CrossPkgCallers != 1 {
		t.Errorf("c.CrossPkgCallers = %d, want 1 (only pkg P)", got["c"].CrossPkgCallers)
	}
	if got["b"].InDegree != 1 {
		t.Errorf("b.InDegree = %d, want 1", got["b"].InDegree)
	}
	if got["b"].CrossPkgCallers != 0 {
		t.Errorf("b.CrossPkgCallers = %d, want 0 (a, b both in P)", got["b"].CrossPkgCallers)
	}
	if got["a"].OutDegree != 2 {
		t.Errorf("a.OutDegree = %d, want 2", got["a"].OutDegree)
	}
	// PageRank should sum to ~1.0 (probability distribution).
	var total float64
	for _, n := range nodes {
		total += got[n.ID].PageRank
	}
	if math.Abs(total-1.0) > 1e-6 {
		t.Errorf("pagerank sum = %v, want ~1.0", total)
	}
	// c has the most callers from the most packages, so it must have
	// the highest PageRank among reachable nodes.
	for _, id := range []string{"a", "b", "d", "e"} {
		if got["c"].PageRank <= got[id].PageRank {
			t.Errorf("pagerank: c (%v) should be > %s (%v)", got["c"].PageRank, id, got[id].PageRank)
		}
	}
}

// TestComputeCentralityDocGraph checks the markdown doc graph folds into
// centrality: in-degree = backlink count, a section link projects onto
// its parent doc, and structural edges (contains/tagged) are excluded.
//
//	a.md --links-->     c.md
//	b.md --wikilinks--> c.md
//	a.md --links-->     c.md#sec   (heading → projects to c.md, dedups vs a→c)
//	c.md --contains-->  c.md#sec   (structural — ignored)
//	a.md --tagged-->    #spec      (structural — ignored)
func TestComputeCentralityDocGraph(t *testing.T) {
	docID := func(rel string) string { return NodeID("", mdPkg, NodeDocument, rel) }
	headID := NodeID("", mdPkg, NodeHeading, "c.md#sec")
	tagID := NodeID("", mdPkg, NodeTag, "spec")
	nodes := []Node{
		{ID: docID("a.md"), Kind: NodeDocument, PackagePath: mdPkg, FilePath: "a.md", QualifiedName: "a.md"},
		{ID: docID("b.md"), Kind: NodeDocument, PackagePath: mdPkg, FilePath: "b.md", QualifiedName: "b.md"},
		{ID: docID("c.md"), Kind: NodeDocument, PackagePath: mdPkg, FilePath: "c.md", QualifiedName: "c.md"},
		{ID: headID, Kind: NodeHeading, PackagePath: mdPkg, FilePath: "c.md", QualifiedName: "c.md#sec"},
		{ID: tagID, Kind: NodeTag, PackagePath: mdPkg, QualifiedName: "spec"},
	}
	edges := []Edge{
		{Kind: EdgeLinks, SrcID: docID("a.md"), DstID: docID("c.md")},
		{Kind: EdgeWikilinks, SrcID: docID("b.md"), DstID: docID("c.md")},
		{Kind: EdgeLinks, SrcID: docID("a.md"), DstID: headID, StartLine: 5},
		{Kind: EdgeContains, SrcID: docID("c.md"), DstID: headID},
		{Kind: EdgeTagged, SrcID: docID("a.md"), DstID: tagID},
	}

	got := ComputeCentrality(nodes, edges)

	// c.md is referenced by a.md (direct + section, deduped) and b.md → 2.
	if got[docID("c.md")].InDegree != 2 {
		t.Errorf("c.md InDegree = %d, want 2 (a.md + b.md, section link deduped)", got[docID("c.md")].InDegree)
	}
	// a.md's two references both target c.md → out-degree 1.
	if got[docID("a.md")].OutDegree != 1 {
		t.Errorf("a.md OutDegree = %d, want 1", got[docID("a.md")].OutDegree)
	}
	// The section link projected to c.md, so the heading itself gets no
	// in-degree; the contains edge is excluded too.
	if got[headID].InDegree != 0 {
		t.Errorf("heading InDegree = %d, want 0 (section link projects to parent; contains excluded)", got[headID].InDegree)
	}
	// The tag is reached only by an excluded `tagged` edge.
	if got[tagID].InDegree != 0 {
		t.Errorf("tag InDegree = %d, want 0 (tagged edge excluded from centrality)", got[tagID].InDegree)
	}
	// Most-referenced doc ranks highest.
	if !(got[docID("c.md")].PageRank > got[docID("a.md")].PageRank && got[docID("c.md")].PageRank > got[docID("b.md")].PageRank) {
		t.Errorf("c.md should outrank its referrers; got c=%v a=%v b=%v",
			got[docID("c.md")].PageRank, got[docID("a.md")].PageRank, got[docID("b.md")].PageRank)
	}
}

// TestComputeCentralityEmpty exercises the zero-edge case — no calls
// edges means nothing to rank, and the function should return an empty
// map rather than NaN-filled noise.
func TestComputeCentralityEmpty(t *testing.T) {
	nodes := []Node{{ID: "a", Kind: NodeFunction, PackagePath: "P"}}
	got := ComputeCentrality(nodes, nil)
	if len(got) != 0 {
		t.Errorf("got %d entries for empty edges, want 0", len(got))
	}
}

// TestComputeCentralitySkipsDanglingEdges confirms edges whose endpoints
// aren't in the node set are dropped silently (cross-module calls into
// std lib, etc.) instead of inventing ghost nodes.
func TestComputeCentralitySkipsDanglingEdges(t *testing.T) {
	nodes := []Node{{ID: "a", Kind: NodeFunction, PackagePath: "P"}}
	edges := []Edge{
		{Kind: EdgeCalls, SrcID: "a", DstID: "missing"},
		{Kind: EdgeCalls, SrcID: "ghost", DstID: "a"},
	}
	got := ComputeCentrality(nodes, edges)
	if len(got) != 0 {
		t.Errorf("got %d entries with all-dangling edges, want 0", len(got))
	}
}

// TestRunPersistsCentrality exercises the full Indexer.Run path against
// the `simple` fixture so the centrality columns actually land in the
// store. Asserts that at least one node ends up with non-zero in_degree,
// which catches both the computation and the persistence wiring.
func TestRunPersistsCentrality(t *testing.T) {
	root := copyFixture(t, "simple")
	st := openTestStore(t)
	p := resolveTestProject(t, root)

	g := New(p, NewStoreAdapter(st), Options{})
	if _, err := g.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	nodes, err := st.GraphAllNodes(context.Background())
	if err != nil {
		t.Fatalf("GraphAllNodes: %v", err)
	}
	var nonZero int
	for _, n := range nodes {
		if n.InDegree > 0 || n.OutDegree > 0 || n.PageRank > 0 {
			nonZero++
		}
	}
	if nonZero == 0 {
		t.Fatalf("no nodes carry centrality after Run — expected calls-edges in the simple fixture")
	}
}
