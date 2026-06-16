package eval

import "testing"

// mkReport builds a Report whose Queries carry the given NDCG values, one query
// per value with a stable ID. Other metrics mirror NDCG so the gate sees a
// consistent signal; tests that care about a specific metric read NDCG@k.
func mkReport(ndcgs ...float64) Report {
	qs := make([]QueryResult, len(ndcgs))
	for i, v := range ndcgs {
		qs[i] = QueryResult{
			ID:         string(rune('a' + i)),
			NDCG:       v,
			Recall:     v,
			RecallPool: v,
			RR:         v,
		}
	}
	return Report{Queries: qs}
}

func ndcgReg(regs []Regression) *Regression {
	for i := range regs {
		if regs[i].Metric == "NDCG@k" {
			return &regs[i]
		}
	}
	return nil
}

func TestBootstrap_NoChangeNoRegression(t *testing.T) {
	r := mkReport(0.5, 0.6, 0.7, 0.8)
	regs, diag := r.BootstrapRegressions(r, DefaultBootstrapParams())
	if diag.Paired != 4 {
		t.Fatalf("paired = %d, want 4", diag.Paired)
	}
	if len(regs) != 0 {
		t.Fatalf("identical reports flagged regressions: %v", regs)
	}
}

func TestBootstrap_UniformDropFlagged(t *testing.T) {
	ref := mkReport(0.5, 0.6, 0.7, 0.8, 0.9)
	now := mkReport(0.4, 0.5, 0.6, 0.7, 0.8) // every query -0.1
	regs, _ := now.BootstrapRegressions(ref, DefaultBootstrapParams())
	r := ndcgReg(regs)
	if r == nil {
		t.Fatalf("uniform -0.1 drop not flagged: %v", regs)
	}
	if r.CIHigh >= 0 {
		t.Fatalf("CI upper bound %.3f should be < 0 for a confident drop", r.CIHigh)
	}
}

func TestBootstrap_NoiseNotFlagged(t *testing.T) {
	// Deltas centred near zero with spread — net change ~0, must NOT flag.
	ref := mkReport(0.5, 0.5, 0.5, 0.5, 0.5, 0.5)
	now := mkReport(0.4, 0.6, 0.3, 0.7, 0.45, 0.55) // mean delta ≈ 0
	regs, _ := now.BootstrapRegressions(ref, DefaultBootstrapParams())
	if r := ndcgReg(regs); r != nil {
		t.Fatalf("noise around zero flagged as regression: CI %.3f..%.3f", r.CILow, r.CIHigh)
	}
}

func TestBootstrap_SmallConfidentDropFlagged(t *testing.T) {
	// A drop well under the old fixed 0.02 cliff, but consistent across many
	// queries (low variance) — the bootstrap should catch what 0.02 missed.
	n := 200
	refv := make([]float64, n)
	nowv := make([]float64, n)
	for i := 0; i < n; i++ {
		refv[i] = 0.50
		nowv[i] = 0.49 // uniform -0.01, below the old tol
	}
	regs, _ := mkReport(nowv...).BootstrapRegressions(mkReport(refv...), DefaultBootstrapParams())
	if ndcgReg(regs) == nil {
		t.Fatalf("consistent -0.01 drop (n=200) not flagged — bootstrap should beat fixed 0.02")
	}
}

func TestBootstrap_MinEffectFloorSuppresses(t *testing.T) {
	n := 200
	refv := make([]float64, n)
	nowv := make([]float64, n)
	for i := 0; i < n; i++ {
		refv[i] = 0.50
		nowv[i] = 0.49 // -0.01, confident but tiny
	}
	p := DefaultBootstrapParams()
	p.MinEffect = 0.02 // ignore drops whose point estimate is below 0.02
	regs, _ := mkReport(nowv...).BootstrapRegressions(mkReport(refv...), p)
	if ndcgReg(regs) != nil {
		t.Fatalf("MinEffect floor 0.02 should suppress a -0.01 drop")
	}
}

func TestBootstrap_UnpairedFallbackSignal(t *testing.T) {
	ref := Report{Queries: []QueryResult{{ID: "x", NDCG: 0.5}}}
	now := Report{Queries: []QueryResult{{ID: "y", NDCG: 0.1}}}
	regs, diag := now.BootstrapRegressions(ref, DefaultBootstrapParams())
	if diag.Paired != 0 {
		t.Fatalf("paired = %d, want 0 (disjoint IDs)", diag.Paired)
	}
	if diag.OnlyNow != 1 || diag.OnlyRef != 1 {
		t.Fatalf("diag = %+v, want OnlyNow=1 OnlyRef=1", diag)
	}
	if regs != nil {
		t.Fatalf("no pairs should yield nil regressions, got %v", regs)
	}
}

func TestBootstrap_Deterministic(t *testing.T) {
	ref := mkReport(0.5, 0.6, 0.7, 0.8, 0.9)
	now := mkReport(0.45, 0.5, 0.68, 0.72, 0.8)
	a, _ := now.BootstrapRegressions(ref, DefaultBootstrapParams())
	b, _ := now.BootstrapRegressions(ref, DefaultBootstrapParams())
	if len(a) != len(b) {
		t.Fatalf("non-deterministic regression count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic CI: %+v vs %+v", a[i], b[i])
		}
	}
}
