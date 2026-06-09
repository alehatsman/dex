package eval

import "testing"

func TestCompute(t *testing.T) {
	results := []QueryResult{
		{NDCG: 1.0, Recall: 1.0, RR: 1.0},
		{NDCG: 0.0, Recall: 0.0, RR: 0.0},
	}
	rep := Compute(results, 10)
	if rep.N != 2 {
		t.Fatalf("N: got %d, want 2", rep.N)
	}
	if !approxEq(rep.MeanNDCG, 0.5) || !approxEq(rep.MeanRecall, 0.5) || !approxEq(rep.MRR, 0.5) {
		t.Errorf("means: got ndcg=%v recall=%v mrr=%v, want 0.5 each", rep.MeanNDCG, rep.MeanRecall, rep.MRR)
	}
}

func TestRegressions(t *testing.T) {
	ref := Report{MeanNDCG: 0.60, MeanRecall: 0.64, MRR: 0.75}

	// Within tolerance on all three → no regression.
	ok := Report{MeanNDCG: 0.59, MeanRecall: 0.63, MRR: 0.74}
	if regs := ok.Regressions(ref, 0.02); len(regs) != 0 {
		t.Errorf("within tolerance flagged a regression: %v", regs)
	}

	// A deliberate retrieval regression: NDCG drops well beyond tolerance.
	bad := Report{MeanNDCG: 0.40, MeanRecall: 0.63, MRR: 0.74}
	regs := bad.Regressions(ref, 0.02)
	if len(regs) != 1 || regs[0].Metric != "NDCG@k" {
		t.Fatalf("expected exactly one NDCG@k regression, got %v", regs)
	}

	// An improvement is never a regression.
	better := Report{MeanNDCG: 0.70, MeanRecall: 0.70, MRR: 0.80}
	if regs := better.Regressions(ref, 0.02); len(regs) != 0 {
		t.Errorf("improvement flagged a regression: %v", regs)
	}

	// Multiple metrics regressing are all reported.
	worse := Report{MeanNDCG: 0.40, MeanRecall: 0.40, MRR: 0.40}
	if regs := worse.Regressions(ref, 0.02); len(regs) != 3 {
		t.Errorf("expected all three metrics flagged, got %v", regs)
	}
}
