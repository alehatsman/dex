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

func TestComputeRecallPoolAndByType(t *testing.T) {
	results := []QueryResult{
		{Type: "symbol", NDCG: 1.0, Recall: 1.0, RecallPool: 1.0, RR: 1.0},
		{Type: "symbol", NDCG: 0.0, Recall: 0.0, RecallPool: 1.0, RR: 0.0},
		{Type: "nl", NDCG: 0.5, Recall: 0.5, RecallPool: 0.5, RR: 0.5},
	}
	rep := Compute(results, 10)

	// Pool recall averages independently of top-k recall.
	if !approxEq(rep.MeanRecallPool, (1.0+1.0+0.5)/3) {
		t.Errorf("MeanRecallPool: got %v", rep.MeanRecallPool)
	}
	// Buckets split by query type; sub-reports carry no nested detail.
	if len(rep.ByType) != 2 {
		t.Fatalf("ByType buckets: got %d, want 2", len(rep.ByType))
	}
	sym := rep.ByType["symbol"]
	if sym.N != 2 || !approxEq(sym.MeanRecallPool, 1.0) || !approxEq(sym.MeanNDCG, 0.5) {
		t.Errorf("symbol bucket: %+v", sym)
	}
	if sym.ByType != nil || sym.Queries != nil {
		t.Error("bucket sub-report must not carry ByType or Queries")
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

func TestByTypeRegressions(t *testing.T) {
	// Aggregate is flat, but the `symbol` bucket regresses while `nl` improves
	// — exactly the per-type drop an aggregate-only gate would miss.
	ref := Report{ByType: map[string]Report{
		"nl":     {N: 30, MeanNDCG: 0.60, MeanRecall: 0.70, MRR: 0.75},
		"symbol": {N: 10, MeanNDCG: 0.80, MeanRecall: 0.85, MRR: 0.90},
		"small":  {N: 2, MeanNDCG: 0.90, MeanRecall: 0.90, MRR: 0.90},
	}}
	now := Report{ByType: map[string]Report{
		"nl":     {N: 30, MeanNDCG: 0.65, MeanRecall: 0.72, MRR: 0.78}, // improved
		"symbol": {N: 10, MeanNDCG: 0.50, MeanRecall: 0.85, MRR: 0.90}, // NDCG sank
		"small":  {N: 2, MeanNDCG: 0.10, MeanRecall: 0.10, MRR: 0.10},  // below minBucket → ignored
	}}
	regs, delta := now.ByTypeRegressions(ref, 0.02, 5)
	if len(delta) != 0 {
		t.Fatalf("unexpected bucket delta: %v", delta)
	}
	if len(regs) != 1 || regs[0].Metric != "symbol/NDCG@k" {
		t.Fatalf("expected one symbol/NDCG@k regression, got %v", regs)
	}

	// A bucket present in only one report is surfaced as a delta, not silently dropped.
	now2 := Report{ByType: map[string]Report{
		"nl": {N: 30, MeanNDCG: 0.65, MeanRecall: 0.72, MRR: 0.78},
	}}
	_, delta2 := now2.ByTypeRegressions(ref, 0.02, 5)
	if len(delta2) == 0 {
		t.Fatal("expected bucket-set change to be reported")
	}
}
