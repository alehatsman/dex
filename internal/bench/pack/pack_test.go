package pack

import (
	"math"
	"testing"
)

// fixed cost model: every file reads as 100 tokens, a trace envelope as 10 per
// listed path. Deterministic and index-free.
func fixedCost() CostModel {
	return CostModel{
		Read:          func(string) int { return 100 },
		TraceEnvelope: func(paths []string) int { return 10 * len(paths) },
	}
}

func TestComputePrimitiveCost(t *testing.T) {
	tasks := []Task{{
		Symbol: "pkg.Foo",
		Def:    "pkg/foo.go",
		Gold:   []string{"pkg/foo.go", "pkg/caller.go", "pkg/callee.go", "pkg/foo_test.go"},
	}}
	// pack surfaces everything, cheaply.
	pm := PackModel{Surfaced: func(string) ([]string, int, bool) {
		return []string{"pkg/foo.go", "pkg/caller.go", "pkg/callee.go", "pkg/foo_test.go"}, 120, true
	}}

	rep := Compute(tasks, fixedCost(), pm)
	res := rep.Results[0]

	// primitive calls = 4 fixed (locate + trace callers + trace callees + find
	// tests) + one read per distinct gold (4) = 8.
	if res.PrimitiveCalls != 8 {
		t.Errorf("primitive calls = %d, want 8", res.PrimitiveCalls)
	}
	// primitive tokens = 4 reads*100 + envelope 10*4 = 440.
	if res.PrimitiveTokens != 440 {
		t.Errorf("primitive tokens = %d, want 440", res.PrimitiveTokens)
	}
	if res.PackCalls != 1 || res.PackTokens != 120 {
		t.Errorf("pack calls/tokens = %d/%d, want 1/120", res.PackCalls, res.PackTokens)
	}
	if res.Coverage != 1.0 || !res.FullyCovered {
		t.Errorf("coverage = %.2f fully=%v, want 1.0/true", res.Coverage, res.FullyCovered)
	}
	if rep.CallsSavedPct <= 0 || rep.TokensSavedPct <= 0 {
		t.Errorf("expected positive savings, got calls=%.2f tokens=%.2f", rep.CallsSavedPct, rep.TokensSavedPct)
	}
}

func TestPartialCoverageCountsButNotFull(t *testing.T) {
	tasks := []Task{
		{Symbol: "a.Full", Gold: []string{"a.go", "b.go"}},
		{Symbol: "a.Partial", Gold: []string{"c.go", "d.go"}}, // pack drops d.go
	}
	pm := PackModel{Surfaced: func(sym string) ([]string, int, bool) {
		switch sym {
		case "a.Full":
			return []string{"a.go", "b.go"}, 50, true
		case "a.Partial":
			return []string{"c.go"}, 50, true // misses d.go → coverage 0.5
		}
		return nil, 0, false
	}}

	rep := Compute(tasks, fixedCost(), pm)

	if rep.NumHit != 2 {
		t.Errorf("hits = %d, want 2", rep.NumHit)
	}
	// only a.Full fully covers its ripple.
	if rep.NumCovered != 1 {
		t.Errorf("covered = %d, want 1", rep.NumCovered)
	}
	if rep.Results[1].Coverage != 0.5 {
		t.Errorf("partial coverage = %.2f, want 0.5", rep.Results[1].Coverage)
	}
	// mean coverage over reached tasks = (1.0 + 0.5)/2 = 0.75.
	if math.Abs(rep.MeanCoverage-0.75) > 1e-9 {
		t.Errorf("mean coverage = %.3f, want 0.75", rep.MeanCoverage)
	}
	// full-cover rate over reached = 1/2.
	if math.Abs(rep.FullCoverRate-0.5) > 1e-9 {
		t.Errorf("full cover rate = %.3f, want 0.5", rep.FullCoverRate)
	}
}

func TestPackMissCountsAgainstReach(t *testing.T) {
	tasks := []Task{{Symbol: "gone.Sym", Gold: []string{"x.go"}}}
	pm := PackModel{Surfaced: func(string) ([]string, int, bool) { return nil, 0, false }}

	rep := Compute(tasks, fixedCost(), pm)
	if rep.NumHit != 0 || rep.ReachRate != 0 {
		t.Errorf("miss should give 0 reach, got hit=%d rate=%.2f", rep.NumHit, rep.ReachRate)
	}
	if rep.Results[0].FullyCovered {
		t.Error("a miss must not be fully covered")
	}
}

func TestRegressionsGate(t *testing.T) {
	ref := Report{
		MeanCoverage:   0.9,
		FullCoverRate:  0.8,
		ReachRate:      1.0,
		MeanPackCalls:  1.0,
		MeanPackTokens: 100,
	}

	// coverage drop beyond tol → flagged.
	drop := ref
	drop.MeanCoverage = 0.7
	if regs := drop.Regressions(ref, 0.05, 0.05); len(regs) == 0 {
		t.Error("expected a mean_coverage regression")
	}

	// token bloat beyond relTol → flagged.
	bloat := ref
	bloat.MeanPackTokens = 120 // +20% > 5%
	got := bloat.Regressions(ref, 0.05, 0.05)
	found := false
	for _, r := range got {
		if r.Metric == "mean_pack_tokens" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a mean_pack_tokens regression, got %v", got)
	}

	// within tolerance → clean.
	ok := ref
	ok.MeanPackTokens = 103 // +3% < 5%
	ok.MeanCoverage = 0.88  // −0.02 < 0.05
	if regs := ok.Regressions(ref, 0.05, 0.05); len(regs) != 0 {
		t.Errorf("expected no regressions, got %v", regs)
	}
}

func TestEmptyGoldIsFullyCovered(t *testing.T) {
	// a symbol with no derivable neighbours is trivially covered; the CLI
	// filters these out as non-modify-ripple, but the model must not divide by
	// zero.
	if c := coverage(nil, nil); c != 1.0 {
		t.Errorf("empty gold coverage = %.2f, want 1.0", c)
	}
}
