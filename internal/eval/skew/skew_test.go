package skew

import (
	"math"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

func ts(lang string) []byte { return []byte(`{"language":"` + lang + `"}`) }

func mkView(nodes []graphquery.Node) *graphquery.View {
	v := &graphquery.View{NodesByID: map[string]graphquery.Node{}}
	for _, n := range nodes {
		v.NodesByID[n.ID] = n
	}
	return v
}

func find(rep Report, lang string) (LangStat, bool) {
	for _, s := range rep.Languages {
		if s.Language == lang {
			return s, true
		}
	}
	return LangStat{}, false
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Two languages, equal node counts, but Go holds 4x the PageRank mass —
// the canonical gate-2 skew. Go must read >1, the tree-sitter lang <1.
func TestCompute_SkewDetected(t *testing.T) {
	view := mkView([]graphquery.Node{
		// Go: no metadata → "go". 2 nodes, total pr 0.8.
		{ID: "g1", Kind: graph.NodeFunction, PageRank: 0.5, CommunityID: 1},
		{ID: "g2", Kind: graph.NodeMethod, PageRank: 0.3, CommunityID: 1},
		// TS: stamped language. 2 nodes, total pr 0.2.
		{ID: "t1", Kind: graph.NodeFunction, PageRank: 0.15, CommunityID: 2, MetadataJSON: ts("typescript")},
		{ID: "t2", Kind: graph.NodeMethod, PageRank: 0.05, CommunityID: 2, MetadataJSON: ts("typescript")},
	})

	rep := Compute(view)

	if rep.TotalNodes != 4 {
		t.Fatalf("TotalNodes = %d, want 4", rep.TotalNodes)
	}
	if !approx(rep.TotalPageRank, 1.0) {
		t.Fatalf("TotalPageRank = %v, want 1.0", rep.TotalPageRank)
	}
	if rep.TotalCommunities != 2 {
		t.Fatalf("TotalCommunities = %d, want 2", rep.TotalCommunities)
	}

	goSt, ok := find(rep, "go")
	if !ok {
		t.Fatal("missing go stat")
	}
	if !approx(goSt.NodeShare, 0.5) || !approx(goSt.PageRankShare, 0.8) {
		t.Fatalf("go shares: node=%v pr=%v, want 0.5 / 0.8", goSt.NodeShare, goSt.PageRankShare)
	}
	if !approx(goSt.SkewRatio, 1.6) {
		t.Fatalf("go SkewRatio = %v, want 1.6", goSt.SkewRatio)
	}
	if goSt.SkewRatio <= 1 {
		t.Errorf("go should be over-weighted (>1), got %v", goSt.SkewRatio)
	}

	tsSt, ok := find(rep, "typescript")
	if !ok {
		t.Fatal("missing typescript stat")
	}
	if !approx(tsSt.SkewRatio, 0.4) {
		t.Fatalf("ts SkewRatio = %v, want 0.4", tsSt.SkewRatio)
	}
	if tsSt.SkewRatio >= 1 {
		t.Errorf("typescript should be under-weighted (<1), got %v", tsSt.SkewRatio)
	}

	// Sorted by descending PageRankShare: go first.
	if rep.Languages[0].Language != "go" {
		t.Errorf("languages[0] = %q, want go (highest pagerank share)", rep.Languages[0].Language)
	}
}

// Non-call node kinds (types, fields, packages) carry zero centrality by
// design and must not enter the population — they would dilute the shares.
func TestCompute_ExcludesNonCallNodes(t *testing.T) {
	view := mkView([]graphquery.Node{
		{ID: "f", Kind: graph.NodeFunction, PageRank: 1.0},
		{ID: "ty", Kind: graph.NodeType, PageRank: 0, MetadataJSON: ts("rust")},
		{ID: "fld", Kind: graph.NodeField},
		{ID: "pkg", Kind: graph.NodePackage, MetadataJSON: ts("rust")},
	})

	rep := Compute(view)

	if rep.TotalNodes != 1 {
		t.Fatalf("TotalNodes = %d, want 1 (only the function)", rep.TotalNodes)
	}
	if len(rep.Languages) != 1 || rep.Languages[0].Language != "go" {
		t.Fatalf("languages = %+v, want only go", rep.Languages)
	}
}

// A balanced graph (shares track each other) sits near skew 1.0.
func TestCompute_NoSkewNearOne(t *testing.T) {
	view := mkView([]graphquery.Node{
		{ID: "g1", Kind: graph.NodeFunction, PageRank: 0.25},
		{ID: "g2", Kind: graph.NodeFunction, PageRank: 0.25},
		{ID: "p1", Kind: graph.NodeFunction, PageRank: 0.25, MetadataJSON: ts("python")},
		{ID: "p2", Kind: graph.NodeFunction, PageRank: 0.25, MetadataJSON: ts("python")},
	})

	rep := Compute(view)
	for _, s := range rep.Languages {
		if !approx(s.SkewRatio, 1.0) {
			t.Errorf("%s SkewRatio = %v, want ~1.0 for a balanced graph", s.Language, s.SkewRatio)
		}
	}
}

func TestCompute_NilAndEmpty(t *testing.T) {
	if rep := Compute(nil); rep.TotalNodes != 0 || len(rep.Languages) != 0 {
		t.Errorf("Compute(nil) = %+v, want zero report", rep)
	}
	if rep := Compute(mkView(nil)); rep.TotalNodes != 0 || len(rep.Languages) != 0 {
		t.Errorf("Compute(empty) = %+v, want zero report", rep)
	}
}
