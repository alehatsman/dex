package benchnav

import "testing"

// fixedCost prices every read at 10 tokens and every find envelope at 5,
// so calls/tokens are exactly predictable from the policy.
func fixedCost() CostModel {
	return CostModel{
		Read:         func(string) int { return 10 },
		FindEnvelope: func([]string) int { return 5 },
	}
}

func TestCompute_HitAtRank(t *testing.T) {
	// gold "b.go" is at rank 2: find(1 call) + read a.go + read b.go = 3 calls.
	// tokens = 5 (envelope) + 10 + 10 = 25.
	q := []Query{{
		Query:    "q",
		Ranked:   []string{"a.go", "b.go", "c.go"},
		Relevant: []string{"b.go"},
	}}
	rep := Compute(q, 10, fixedCost(), "test")

	if rep.NumReached != 1 || rep.ReachRate != 1.0 {
		t.Fatalf("reach: got reached=%d rate=%.2f, want 1 / 1.00", rep.NumReached, rep.ReachRate)
	}
	r := rep.Results[0]
	if !r.Reached || r.FirstGoldRank != 2 {
		t.Fatalf("rank: got reached=%v rank=%d, want true / 2", r.Reached, r.FirstGoldRank)
	}
	if r.Calls != 3 || r.Tokens != 25 {
		t.Fatalf("cost: got calls=%d tokens=%d, want 3 / 25", r.Calls, r.Tokens)
	}
}

func TestCompute_MissBeyondTopK(t *testing.T) {
	// gold is at rank 3 but k=2, so it is never reached. The agent pays for
	// the full horizon: find + 2 reads = 3 calls, 5 + 20 = 25 tokens. The miss
	// must be excluded from mean/median but counted in reach-rate.
	q := []Query{{
		Query:    "q",
		Ranked:   []string{"a.go", "b.go", "gold.go"},
		Relevant: []string{"gold.go"},
	}}
	rep := Compute(q, 2, fixedCost(), "test")

	if rep.NumReached != 0 || rep.ReachRate != 0.0 {
		t.Fatalf("reach: got reached=%d rate=%.2f, want 0 / 0.00", rep.NumReached, rep.ReachRate)
	}
	if rep.MeanCalls != 0 || rep.MeanTokens != 0 {
		t.Fatalf("aggregates must skip misses: got calls=%.2f tokens=%.2f", rep.MeanCalls, rep.MeanTokens)
	}
	r := rep.Results[0]
	if r.Reached || r.FirstGoldRank != 0 || r.Calls != 3 || r.Tokens != 25 {
		t.Fatalf("miss result: got %+v, want reached=false rank=0 calls=3 tokens=25", r)
	}
}

func TestCompute_HitAtRankOne(t *testing.T) {
	q := []Query{{Query: "q", Ranked: []string{"gold.go", "b.go"}, Relevant: []string{"gold.go"}}}
	rep := Compute(q, 10, fixedCost(), "test")
	r := rep.Results[0]
	if r.FirstGoldRank != 1 || r.Calls != 2 || r.Tokens != 15 {
		t.Fatalf("rank-1: got rank=%d calls=%d tokens=%d, want 1 / 2 / 15", r.FirstGoldRank, r.Calls, r.Tokens)
	}
}

func TestCompute_AggregatesOverReachedOnly(t *testing.T) {
	// Two reached (rank 1 -> 2 calls; rank 3 -> 4 calls) + one miss.
	q := []Query{
		{Query: "a", Ranked: []string{"g1.go", "x.go"}, Relevant: []string{"g1.go"}},
		{Query: "b", Ranked: []string{"x.go", "y.go", "g2.go"}, Relevant: []string{"g2.go"}},
		{Query: "c", Ranked: []string{"x.go", "y.go"}, Relevant: []string{"missing.go"}},
	}
	rep := Compute(q, 10, fixedCost(), "test")

	if rep.NumQueries != 3 || rep.NumReached != 2 {
		t.Fatalf("counts: got q=%d reached=%d, want 3 / 2", rep.NumQueries, rep.NumReached)
	}
	// mean calls over {2,4} = 3; median = 3.
	if rep.MeanCalls != 3 || rep.MedianCalls != 3 {
		t.Fatalf("calls agg: got mean=%.2f median=%.2f, want 3 / 3", rep.MeanCalls, rep.MedianCalls)
	}
	if rep.ReachRate < 0.66 || rep.ReachRate > 0.67 {
		t.Fatalf("reach rate: got %.4f, want ~0.6667", rep.ReachRate)
	}
}

func TestRegressions(t *testing.T) {
	ref := Report{ReachRate: 0.80, MeanCalls: 4.0, MeanTokens: 1000}

	// Within tolerance: slightly fewer calls, same reach -> no regression.
	ok := Report{ReachRate: 0.80, MeanCalls: 3.8, MeanTokens: 1000}
	if regs := ok.Regressions(ref, 0.02, 0.05); len(regs) != 0 {
		t.Fatalf("expected no regressions, got %v", regs)
	}

	// Reach drops, calls and tokens both rise past tolerance -> three regressions.
	bad := Report{ReachRate: 0.70, MeanCalls: 4.5, MeanTokens: 1100}
	regs := bad.Regressions(ref, 0.02, 0.05)
	if len(regs) != 3 {
		t.Fatalf("expected 3 regressions, got %d: %v", len(regs), regs)
	}
}

func TestMeanMedian_EvenAndEmpty(t *testing.T) {
	if m, md := meanMedian(nil); m != 0 || md != 0 {
		t.Fatalf("empty: got %.2f/%.2f", m, md)
	}
	// {1,2,3,4} -> mean 2.5, median (2+3)/2 = 2.5
	if m, md := meanMedian([]int{4, 1, 3, 2}); m != 2.5 || md != 2.5 {
		t.Fatalf("even: got mean=%.2f median=%.2f, want 2.5/2.5", m, md)
	}
}
