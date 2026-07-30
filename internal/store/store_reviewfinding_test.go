package store

import "testing"

// TestReviewFindingSalience locks the tuning for the ReviewFinding archetype
// (#87): it is actionable but point-in-time, so it weighs below a Gotcha /
// Architecture yet above a plain Decision, and it decays faster than a Gotcha so
// a stale finding evicts if nobody reaffirms it. A mis-cased or unlisted
// archetype would silently fall back to the defaults (1.0 / 0.010) — these
// assertions guard that regression (#520).
func TestReviewFindingSalience(t *testing.T) {
	const (
		wantWeight = 1.3
		wantDecay  = 0.012
	)
	if w := archetypeWeight("ReviewFinding"); w != wantWeight {
		t.Errorf("archetypeWeight(ReviewFinding) = %v, want %v (not the 1.0 default)", w, wantWeight)
	}
	if d := archetypeDecayRate("ReviewFinding"); d != wantDecay {
		t.Errorf("archetypeDecayRate(ReviewFinding) = %v, want %v (not the 0.010 default)", d, wantDecay)
	}

	// Ordering invariants that give the archetype its meaning.
	if archetypeWeight("ReviewFinding") >= archetypeWeight("Gotcha") {
		t.Error("ReviewFinding must weigh below Gotcha")
	}
	if archetypeWeight("ReviewFinding") <= archetypeWeight("Decision") {
		t.Error("ReviewFinding must weigh above Decision")
	}
	if archetypeDecayRate("ReviewFinding") <= archetypeDecayRate("Gotcha") {
		t.Error("ReviewFinding must decay faster than Gotcha")
	}
}
