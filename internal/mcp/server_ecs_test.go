package mcp

import (
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func TestUniqueWordRatio(t *testing.T) {
	cases := []struct {
		text string
		want float32
	}{
		{"", 0},
		{"a a a a", 0.25},       // 1 unique / 4 total
		{"a b c d", 1.0},        // 4 unique / 4 total
		{"foo bar foo baz", 0.75}, // 3 unique / 4 total
	}
	for _, c := range cases {
		got := uniqueWordRatio(c.text)
		if got != c.want {
			t.Errorf("uniqueWordRatio(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestTaskRelevance_Empty(t *testing.T) {
	r := taskRelevance("anything", "sym", nil)
	if r != 0.5 {
		t.Errorf("empty task KWs: want 0.5, got %v", r)
	}
}

func TestTaskRelevance_Match(t *testing.T) {
	kws := []string{"loop", "detector"}
	r := taskRelevance("the loop detector catches spinning agents", "loopDetector", kws)
	if r != 1.0 {
		t.Errorf("full match: want 1.0, got %v", r)
	}
}

func TestTaskRelevance_Partial(t *testing.T) {
	kws := []string{"loop", "detector", "unknown"}
	r := taskRelevance("loop only here", "sym", kws)
	const want = float32(1) / 3
	if r != want {
		t.Errorf("partial match: want %v, got %v", want, r)
	}
}

func TestGraphCentrality(t *testing.T) {
	if graphCentrality(0, 0, 10) != 0 {
		t.Error("zero edges → 0")
	}
	if graphCentrality(10, 0, 10) != 1.0 {
		t.Error("max edges → 1.0")
	}
	if graphCentrality(5, 0, 10) != 0.5 {
		t.Error("half max → 0.5")
	}
	if graphCentrality(1, 0, 0) != 0 {
		t.Error("maxEdges=0 → 0 (no div-zero)")
	}
}

func TestJaccardSim(t *testing.T) {
	a := wordTokens("foo bar baz")
	b := wordTokens("bar baz qux")
	// inter=2 {bar,baz}, union=4 {foo,bar,baz,qux}
	got := jaccardSim(a, b)
	const want = float32(2) / 4
	if got != want {
		t.Errorf("jaccard: want %v, got %v", want, got)
	}

	// identical sets
	if jaccardSim(a, a) != 1.0 {
		t.Error("identical sets: want 1.0")
	}
	// disjoint sets
	c := wordTokens("x y z")
	d := wordTokens("a b c")
	if jaccardSim(c, d) != 0 {
		t.Error("disjoint sets: want 0")
	}
}

func TestExtractTaskKWs(t *testing.T) {
	kws := extractTaskKWs("Implement the loop detector for search tools")
	// "the" and "for" are stop words; everything else >= 3 chars passes
	want := map[string]bool{"implement": true, "loop": true, "detector": true, "search": true, "tools": true}
	if len(kws) != len(want) {
		t.Fatalf("got %v, want keys %v", kws, want)
	}
	for _, k := range kws {
		if !want[k] {
			t.Errorf("unexpected keyword %q", k)
		}
	}
}

func TestECSRerank_Empty(t *testing.T) {
	if got := ecsRerank(nil, nil); got != nil {
		t.Error("nil input → nil output")
	}
	single := []store.Hit{{Path: "a.go", Content: "foo"}}
	if got := ecsRerank(single, nil); len(got) != 1 {
		t.Error("single hit → passthrough")
	}
}

func TestECSRerank_Diversity(t *testing.T) {
	// c.go matches task keywords → high ECS → promoted to 1st by ECS scoring.
	// a.go and b.go share identical content (Jaccard=1); b.go loses on graph
	// centrality, so it gets penalised by MIG redundancy and ranks last.
	hits := []store.Hit{
		{Path: "a.go", Content: "foo bar baz qux alpha beta gamma delta epsilon zeta", RRFScore: 0.9, InDegree: 5, OutDegree: 5},
		{Path: "b.go", Content: "foo bar baz qux alpha beta gamma delta epsilon zeta", RRFScore: 0.8, InDegree: 1, OutDegree: 1},
		{Path: "c.go", Content: "search loop detect throttle window fingerprint hash", RRFScore: 0.7, InDegree: 3, OutDegree: 3},
	}
	result := ecsRerank(hits, []string{"search", "loop"})
	if len(result) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result))
	}
	// c.go has full task-relevance (both KWs present) → highest ECS → ranked 1st.
	if result[0].Path != "c.go" {
		t.Errorf("task-relevant c.go should be ranked 1st by ECS, got %s", result[0].Path)
	}
	// a.go is diverse from c.go (disjoint word sets) → good MIG score → 2nd.
	if result[1].Path != "a.go" {
		t.Errorf("diverse a.go should be ranked 2nd by MIG, got %s", result[1].Path)
	}
	// b.go is a near-clone of a.go → high Jaccard redundancy → ranked last.
	if result[2].Path != "b.go" {
		t.Errorf("near-clone b.go should be ranked last, got %s", result[2].Path)
	}
}
