package nav

import (
	"math"
	"testing"
)

// fixedReorientCost: find envelope is one token per ranked path; every file
// reads for a flat 100 tokens. Deterministic, so the arithmetic is checkable.
func fixedReorientCost() CostModel {
	return CostModel{
		Read:         func(string) int { return 100 },
		FindEnvelope: func(ranked []string) int { return len(ranked) },
	}
}

// fixedReorientModel: each working-set file costs 100 tokens in the recap
// digest, budget 250 — so only two of three files fit (coverage truncates).
func fixedReorientModel() ReorientModel {
	return ReorientModel{
		RecapBudget: 250,
		Entry:       func(string) int { return 100 },
	}
}

// sessionAB is one work session of two find()s. Working set = {a,b,c}: a,b
// reached through q1, c through q2. q1 also ranks a non-gold "x" between them.
func sessionAB() ReorientTask {
	return ReorientTask{
		Task:    "session 1",
		Working: []string{"a", "b", "c"},
		Queries: []ReorientQuery{
			{Ranked: []string{"a", "x", "b"}, Gold: []string{"a", "b"}},
			{Ranked: []string{"c", "a"}, Gold: []string{"c"}},
		},
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestComputeReorient_RestoreArithmetic(t *testing.T) {
	rep := ComputeReorient([]ReorientTask{sessionAB()}, 10, fixedReorientCost(), fixedReorientModel(), "full")
	r := rep.Results[0]

	if r.Working != 3 {
		t.Fatalf("working = %d, want 3", r.Working)
	}
	// Re-exploration replays both finds, re-reading until each query's gold is
	// re-touched: q1 reads a,x,b (3); q2 reads c (1). Tokens = envelopes (3+2) +
	// reads (4*100). Calls = 2 finds + 4 reads = 6. Coverage 100% by construction.
	if r.ReexploreTokens != 3+2+400 {
		t.Errorf("reexplore tokens = %d, want 405", r.ReexploreTokens)
	}
	if r.ReexploreCalls != 6 {
		t.Errorf("reexplore calls = %d, want 6", r.ReexploreCalls)
	}
	if r.ReexploreFound != 3 || !approxEq(r.ReexploreCoverage, 1.0) {
		t.Errorf("reexplore coverage = %d/%.3f, want 3/1.0", r.ReexploreFound, r.ReexploreCoverage)
	}
	// Recap: one call, budget 250 fits two 100-token entries, third overflows.
	if r.RecapCalls != 1 {
		t.Errorf("recap calls = %d, want 1", r.RecapCalls)
	}
	if r.RecapTokens != 200 || r.RecapFound != 2 {
		t.Errorf("recap = %d tokens / %d files, want 200/2", r.RecapTokens, r.RecapFound)
	}
	if !approxEq(r.RecapCoverage, 2.0/3.0) {
		t.Errorf("recap coverage = %.3f, want 0.667", r.RecapCoverage)
	}
	if !approxEq(rep.DeltaTokens, 200-405) {
		t.Errorf("delta tokens = %.1f, want -205", rep.DeltaTokens)
	}
}

func TestReorientRegressions_CleanAgainstSelf(t *testing.T) {
	rep := ComputeReorient([]ReorientTask{sessionAB()}, 10, fixedReorientCost(), fixedReorientModel(), "full")
	if regs := rep.Regressions(rep, 0.02, 0.05); len(regs) != 0 {
		t.Fatalf("self-check should be clean, got %v", regs)
	}
}

func TestReorientRegressions_CoverageFloor(t *testing.T) {
	cur := ReorientReport{MeanRecapCoverage: 0.50, MeanReexploreTokens: 400, DeltaTokens: -200}
	ref := ReorientReport{MeanRecapCoverage: 0.66, MeanReexploreTokens: 400, DeltaTokens: -200}
	regs := cur.Regressions(ref, 0.02, 0.05)
	if len(regs) != 1 || regs[0].Metric != "reorient_recap_coverage" {
		t.Fatalf("want a reorient_recap_coverage regression, got %v", regs)
	}
}

func TestReorientRegressions_AdvantageErosion(t *testing.T) {
	// ref: recap saved 205 tokens (material, > 5% of 405). cur: only 100 saved
	// — the edge eroded past the 5% relTol band, so the gate must fire.
	ref := ReorientReport{MeanRecapCoverage: 0.66, MeanReexploreTokens: 405, DeltaTokens: -205}
	cur := ReorientReport{MeanRecapCoverage: 0.66, MeanReexploreTokens: 405, DeltaTokens: -100}
	regs := cur.Regressions(ref, 0.02, 0.05)
	if len(regs) != 1 || regs[0].Metric != "reorient_advantage_tokens" {
		t.Fatalf("want a reorient_advantage_tokens regression, got %v", regs)
	}
}

func TestReorientRegressions_ImmaterialAdvantageIgnored(t *testing.T) {
	// ref advantage is tiny (4 tokens, under 5% of 405), so erosion is noise —
	// the gate must NOT fire even though the edge vanished.
	ref := ReorientReport{MeanRecapCoverage: 0.66, MeanReexploreTokens: 405, DeltaTokens: -4}
	cur := ReorientReport{MeanRecapCoverage: 0.66, MeanReexploreTokens: 405, DeltaTokens: 0}
	if regs := cur.Regressions(ref, 0.02, 0.05); len(regs) != 0 {
		t.Fatalf("immaterial advantage should not gate, got %v", regs)
	}
}
