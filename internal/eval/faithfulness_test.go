package eval

import "testing"

func TestScoreFaithfulness_GroundedAnswer(t *testing.T) {
	evidence := "The store schema is versioned. EnsureEmbedModel guards the index. " +
		"FTS5 provides BM25 search via mattn go-sqlite3."
	answer := "The store schema is versioned. EnsureEmbedModel guards the index against engine mismatch."
	r := ScoreFaithfulness("q1", answer, evidence, DefaultFaithfulnessOpts())
	if r.NumScored != 2 {
		t.Fatalf("NumScored: got %d, want 2", r.NumScored)
	}
	if r.Score != 1.0 {
		t.Errorf("grounded answer should score 1.0, got %.2f (ungrounded=%v)", r.Score, r.Ungrounded)
	}
}

func TestScoreFaithfulness_FabricatedClaim(t *testing.T) {
	evidence := "The store schema is versioned. EnsureEmbedModel guards the index."
	// Second sentence invents identifiers absent from the evidence.
	answer := "The store schema is versioned. " +
		"Redis caching accelerates kubernetes autoscaling through grpc streaming pipelines."
	r := ScoreFaithfulness("q2", answer, evidence, DefaultFaithfulnessOpts())
	if r.NumScored != 2 || r.NumGrounded != 1 {
		t.Fatalf("got scored=%d grounded=%d, want 2/1", r.NumScored, r.NumGrounded)
	}
	if r.Score != 0.5 {
		t.Errorf("score: got %.2f, want 0.50", r.Score)
	}
	if len(r.Ungrounded) != 1 {
		t.Fatalf("want 1 ungrounded claim, got %d: %v", len(r.Ungrounded), r.Ungrounded)
	}
}

func TestScoreFaithfulness_EmptyAndShort(t *testing.T) {
	ev := "anything at all here"
	if r := ScoreFaithfulness("e", "", ev, DefaultFaithfulnessOpts()); r.NumScored != 0 || r.Score != 1.0 {
		t.Errorf("empty answer: got scored=%d score=%.2f, want 0/1.0", r.NumScored, r.Score)
	}
	// All sentences below MinTokens content tokens → nothing scorable.
	if r := ScoreFaithfulness("s", "Here it is. OK. Done.", ev, DefaultFaithfulnessOpts()); r.NumScored != 0 {
		t.Errorf("short sentences should not be scored, got %d", r.NumScored)
	}
}

func TestScoreFaithfulness_NoEvidenceIsNA(t *testing.T) {
	r := ScoreFaithfulness("n", "Some substantive claim about indexing internals here.", "", DefaultFaithfulnessOpts())
	if r.NumScored != 0 || r.Score != 1.0 {
		t.Errorf("no evidence → N/A: got scored=%d score=%.2f, want 0/1.0", r.NumScored, r.Score)
	}
}

func TestScoreFaithfulness_IdentifierTokensKept(t *testing.T) {
	// fts5 / schema_version are short but identifier-shaped; they must be scored,
	// not dropped as noise.
	evidence := "BM25 search needs fts5. The schema_version field gates reindex."
	answer := "Search depends on fts5 and the schema_version field."
	r := ScoreFaithfulness("id", answer, evidence, DefaultFaithfulnessOpts())
	if r.NumScored != 1 || r.NumGrounded != 1 {
		t.Errorf("identifier claim should ground: got scored=%d grounded=%d", r.NumScored, r.NumGrounded)
	}
}

func TestAggregateFaithfulness(t *testing.T) {
	results := []FaithfulnessResult{
		{ID: "a", Score: 1.0, NumScored: 4, NumGrounded: 4},
		{ID: "b", Score: 0.5, NumScored: 2, NumGrounded: 1},
		{ID: "c", Score: 1.0, NumScored: 0}, // N/A — excluded from mean
	}
	rep := AggregateFaithfulness(results, DefaultFaithfulnessOpts())
	if rep.NumAnswers != 3 || rep.NumScorable != 2 {
		t.Fatalf("counts: got answers=%d scorable=%d, want 3/2", rep.NumAnswers, rep.NumScorable)
	}
	if rep.MeanScore != 0.75 {
		t.Errorf("mean over scorable: got %.3f, want 0.750", rep.MeanScore)
	}
	if rep.TotalGrounded != 5 || rep.TotalScored != 6 {
		t.Errorf("totals: got %d/%d, want 5/6", rep.TotalGrounded, rep.TotalScored)
	}
}

func TestAggregateFaithfulness_AllNA(t *testing.T) {
	rep := AggregateFaithfulness([]FaithfulnessResult{{ID: "x", NumScored: 0}}, DefaultFaithfulnessOpts())
	if rep.MeanScore != 1.0 || rep.NumScorable != 0 {
		t.Errorf("all-N/A: got mean=%.2f scorable=%d, want 1.0/0", rep.MeanScore, rep.NumScorable)
	}
}
