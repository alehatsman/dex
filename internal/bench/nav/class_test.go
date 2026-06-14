package nav

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		lex, graph bool
		want       string
	}{
		{true, true, ClassKeyword},  // lexical wins regardless of graph
		{true, false, ClassKeyword}, // keyword-findable
		{false, true, ClassGraph},   // graph-only (import/call chain)
		{false, false, ClassHidden}, // hidden: no static lane reaches it
	}
	for _, c := range cases {
		if got := Classify(c.lex, c.graph); got != c.want {
			t.Errorf("Classify(lex=%v graph=%v): got %s, want %s", c.lex, c.graph, got, c.want)
		}
	}
}

func TestComputeByClass(t *testing.T) {
	// G1: reached at rank 1 (find + 1 read = 2 calls, 15 tokens).
	// G2: reached at rank 2 (find + 2 reads = 3 calls, 25 tokens).
	// G3: gold beyond top-k, never reached — counts in reach-rate, excluded
	//     from mean calls/tokens (the hidden-dependency bucket).
	q := []Query{
		{Query: "g1", Ranked: []string{"a.go"}, Relevant: []string{"a.go"}, Class: ClassKeyword},
		{Query: "g2", Ranked: []string{"x.go", "b.go"}, Relevant: []string{"b.go"}, Class: ClassGraph},
		{Query: "g3", Ranked: []string{"x.go", "y.go"}, Relevant: []string{"z.go"}, Class: ClassHidden},
	}
	rep := Compute(q, 10, fixedCost(), "test")

	if len(rep.ByClass) != 3 {
		t.Fatalf("ByClass len: got %d, want 3", len(rep.ByClass))
	}
	// Ordering must be G1, G2, G3.
	if rep.ByClass[0].Class != ClassKeyword || rep.ByClass[1].Class != ClassGraph || rep.ByClass[2].Class != ClassHidden {
		t.Fatalf("order: got %s/%s/%s, want G1/G2/G3",
			rep.ByClass[0].Class, rep.ByClass[1].Class, rep.ByClass[2].Class)
	}

	g1 := rep.ByClass[0]
	if g1.NumReached != 1 || g1.ReachRate != 1.0 || g1.MeanCalls != 2 || g1.MeanTokens != 15 {
		t.Errorf("G1: got reached=%d rate=%.2f calls=%.1f tokens=%.0f, want 1/1.00/2/15",
			g1.NumReached, g1.ReachRate, g1.MeanCalls, g1.MeanTokens)
	}
	g3 := rep.ByClass[2]
	if g3.NumQueries != 1 || g3.NumReached != 0 || g3.ReachRate != 0 {
		t.Errorf("G3: got queries=%d reached=%d rate=%.2f, want 1/0/0.00 (hidden-dep gap)",
			g3.NumQueries, g3.NumReached, g3.ReachRate)
	}
	if g3.MeanCalls != 0 || g3.MeanTokens != 0 {
		t.Errorf("G3 unreached must not contribute calls/tokens: got calls=%.1f tokens=%.0f",
			g3.MeanCalls, g3.MeanTokens)
	}
}

// Unclassified runs must render and serialize exactly as before — ByClass nil.
func TestComputeByClass_AbsentWhenUnclassified(t *testing.T) {
	q := []Query{{Query: "q", Ranked: []string{"a.go"}, Relevant: []string{"a.go"}}}
	rep := Compute(q, 10, fixedCost(), "test")
	if rep.ByClass != nil {
		t.Fatalf("ByClass must be nil for unclassified queries, got %v", rep.ByClass)
	}
}

// Unknown class labels sort after the known three, alphabetically, so a typo'd
// or future class never panics or reorders the known buckets.
func TestComputeByClass_UnknownClassOrdering(t *testing.T) {
	q := []Query{
		{Query: "z", Ranked: []string{"a.go"}, Relevant: []string{"a.go"}, Class: "Zeta"},
		{Query: "g2", Ranked: []string{"a.go"}, Relevant: []string{"a.go"}, Class: ClassGraph},
		{Query: "a", Ranked: []string{"a.go"}, Relevant: []string{"a.go"}, Class: "Alpha"},
	}
	rep := Compute(q, 10, fixedCost(), "test")
	got := []string{rep.ByClass[0].Class, rep.ByClass[1].Class, rep.ByClass[2].Class}
	want := []string{ClassGraph, "Alpha", "Zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
		}
	}
}
