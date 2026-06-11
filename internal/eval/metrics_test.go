package eval

import (
	"math"
	"testing"
)

func rel(files ...string) map[string]bool {
	m := make(map[string]bool, len(files))
	for _, f := range files {
		m[f] = true
	}
	return m
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNDCG(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}

	// Single relevant at rank 1 → perfect NDCG (DCG==IDCG).
	if got := NDCG(ranked, rel("a"), 4); !approxEq(got, 1.0) {
		t.Errorf("rank-1 relevant: got %v, want 1.0", got)
	}

	// Single relevant at rank 2: DCG = 1/log2(3); IDCG = 1/log2(2) = 1.
	want := 1.0 / math.Log2(3)
	if got := NDCG(ranked, rel("b"), 4); !approxEq(got, want) {
		t.Errorf("rank-2 relevant: got %v, want %v", got, want)
	}

	// Relevant item beyond k is not counted.
	if got := NDCG(ranked, rel("d"), 2); !approxEq(got, 0) {
		t.Errorf("relevant beyond k: got %v, want 0", got)
	}

	// No relevant items → 0, no panic.
	if got := NDCG(ranked, rel(), 4); got != 0 {
		t.Errorf("empty relevant: got %v, want 0", got)
	}

	// Two relevant, both in top-2 but reversed order: DCG with both present
	// at ranks 1,2 equals IDCG → 1.0.
	if got := NDCG(ranked, rel("a", "b"), 4); !approxEq(got, 1.0) {
		t.Errorf("two relevant at top: got %v, want 1.0", got)
	}
}

func TestRecallAtK(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}
	if got := RecallAtK(ranked, rel("a", "c"), 4); !approxEq(got, 1.0) {
		t.Errorf("both in top-4: got %v, want 1.0", got)
	}
	if got := RecallAtK(ranked, rel("a", "z"), 4); !approxEq(got, 0.5) {
		t.Errorf("half present: got %v, want 0.5", got)
	}
	if got := RecallAtK(ranked, rel("c"), 2); !approxEq(got, 0) {
		t.Errorf("relevant beyond k: got %v, want 0", got)
	}
	if got := RecallAtK(ranked, rel(), 4); got != 0 {
		t.Errorf("empty relevant: got %v, want 0", got)
	}
}

func TestMRR(t *testing.T) {
	ranked := []string{"a", "b", "c"}
	if got := MRR(ranked, rel("a")); !approxEq(got, 1.0) {
		t.Errorf("first relevant: got %v, want 1.0", got)
	}
	if got := MRR(ranked, rel("c")); !approxEq(got, 1.0/3.0) {
		t.Errorf("third relevant: got %v, want %v", got, 1.0/3.0)
	}
	// First relevant wins even when later ones exist.
	if got := MRR(ranked, rel("b", "c")); !approxEq(got, 0.5) {
		t.Errorf("first of multiple: got %v, want 0.5", got)
	}
	if got := MRR(ranked, rel("z")); got != 0 {
		t.Errorf("none present: got %v, want 0", got)
	}
}
