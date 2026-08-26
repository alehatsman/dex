package retrieve

import "testing"

func TestConfidenceLevel(t *testing.T) {
	const strong = HighConfidenceScore + 0.1 // >= high threshold
	const mid = LowConfidenceScore + 0.05    // between low and high threshold
	const weak = LowConfidenceScore - 0.1    // below low threshold
	cases := []struct {
		name           string
		intent         string
		nSymbols       int
		topSemScore    float32
		graphEdgeCount int
		hasBlame       bool
		want           string
	}{
		{"symbols carry it", IntentBehaviorSearch, 3, weak, 0, false, "high"},
		{"strong semantic", IntentBehaviorSearch, 0, strong, 0, false, "high"},
		{"mid semantic no symbols", IntentBehaviorSearch, 0, mid, 0, false, "medium"},
		{"weak semantic no symbols", IntentBehaviorSearch, 0, weak, 0, false, "low"},
		{"no evidence at all", IntentBehaviorSearch, 0, 0, 0, false, "low"},
		{"topology payload rescues weak", IntentPackageTopology, 0, weak, 5, false, "high"},
		{"topology payload absent", IntentPackageTopology, 0, weak, 0, false, "low"},
		{"architecture payload rescues weak", IntentArchitecture, 0, weak, 2, false, "high"},
		{"editing blame rescues weak", IntentEditingContext, 0, weak, 0, true, "high"},
		{"editing no blame", IntentEditingContext, 0, weak, 0, false, "low"},
		{"low threshold is inclusive", IntentBehaviorSearch, 0, LowConfidenceScore, 0, false, "medium"},
		{"high threshold is inclusive", IntentBehaviorSearch, 0, HighConfidenceScore, 0, false, "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConfidenceLevel(c.intent, c.nSymbols, c.topSemScore, c.graphEdgeCount, c.hasBlame)
			if got != c.want {
				t.Errorf("ConfidenceLevel(%q, %d, %v, %d, %v) = %q, want %q",
					c.intent, c.nSymbols, c.topSemScore, c.graphEdgeCount, c.hasBlame, got, c.want)
			}
		})
	}
}

// TestConfidenceMirrorsNextAction pins the invariant that ConfidenceLevel is
// "low" exactly when BuildNextAction emits its weak-semantic prose — the two
// must never drift, since Confidence is the structured mirror of that signal.
func TestConfidenceMirrorsNextAction(t *testing.T) {
	weakMsg := "Top semantic match is weak — rephrase with concrete keywords or fall back to grep."
	// A read exists (so the "nothing found" branch is skipped) but the
	// semantic score is weak and there are no symbols/payload → the weak
	// prose fires, and Confidence must independently read "low".
	reads := []SuggestedRead{{Path: "x.go", StartLine: 1, EndLine: 2}}
	na := BuildNextAction(IntentBehaviorSearch, reads, nil, LowConfidenceScore-0.1, 0, 0, false)
	conf := ConfidenceLevel(IntentBehaviorSearch, 0, LowConfidenceScore-0.1, 0, false)
	if na != weakMsg {
		t.Fatalf("expected weak next_action prose, got %q", na)
	}
	if conf != "low" {
		t.Errorf("Confidence should be low when next_action is the weak-semantic message, got %q", conf)
	}
}
