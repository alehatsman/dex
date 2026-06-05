package compress

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordFeedback_PenaltyAccumulates(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)

	// Three bad turns (ratio=0 → badness=1.0).
	for i := 0; i < 3; i++ {
		pt.RecordFeedback("generate", "aggressive", 0.0)
	}
	// After 3 turns with ema: p = 0*0.9+1*0.1 = 0.1; 0.1*0.9+0.1 = 0.19; 0.19*0.9+0.1 = 0.271
	if pt.penalties["generate"]["aggressive"] <= 0 {
		t.Error("penalty should be positive after bad feedback")
	}
}

func TestRecordFeedback_GoodFeedbackDecaysPenalty(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)

	// Build up a high penalty.
	for i := 0; i < 20; i++ {
		pt.RecordFeedback("generate", "aggressive", 0.0)
	}
	before := pt.penalties["generate"]["aggressive"]

	// Many good turns should decay it.
	for i := 0; i < 30; i++ {
		pt.RecordFeedback("generate", "aggressive", 0.5)
	}
	after := pt.penalties["generate"]["aggressive"]
	if after >= before {
		t.Errorf("penalty should decrease after good feedback: before=%f after=%f", before, after)
	}
}

func TestChooseMode_NoPenalty(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)
	// No feedback → predicted mode returned unchanged.
	if got := pt.ChooseMode("generate", "aggressive"); got != "aggressive" {
		t.Errorf("expected aggressive, got %q", got)
	}
}

func TestChooseMode_PenalizedModeFallsBack(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)

	// Saturate the penalty for aggressive (badness=1.0 × many turns → near 1.0).
	for i := 0; i < 50; i++ {
		pt.RecordFeedback("generate", "aggressive", 0.0)
	}
	got := pt.ChooseMode("generate", "aggressive")
	if got == "aggressive" {
		t.Error("penalized mode should fall back to a cheaper mode")
	}
	// Should be one of the fallbacks.
	allowed := map[string]bool{"signatures": true, "map": true, "full": true}
	if !allowed[got] {
		t.Errorf("unexpected fallback mode %q", got)
	}
}

func TestChooseMode_UnknownIntent(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)
	// Unknown intent → predicted returned.
	if got := pt.ChooseMode("", "aggressive"); got != "aggressive" {
		t.Errorf("empty intent should return predicted, got %q", got)
	}
}

func TestLoadPolicy_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(dir)
	for i := 0; i < 10; i++ {
		pt.RecordFeedback("refactor", "aggressive", 0.0)
	}
	saved := pt.penalties["refactor"]["aggressive"]

	// Load a fresh instance from the same dir.
	pt2 := LoadPolicy(dir)
	if pt2.penalties["refactor"]["aggressive"] != saved {
		t.Errorf("persisted penalty mismatch: want %f got %f", saved, pt2.penalties["refactor"]["aggressive"])
	}
}

func TestLoadPolicy_MissingFile(t *testing.T) {
	dir := t.TempDir()
	pt := LoadPolicy(filepath.Join(dir, "nonexistent"))
	if pt == nil || len(pt.penalties) != 0 {
		t.Error("missing policy file should return empty table")
	}
}

func TestLoadPolicy_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, policyFile)
	_ = os.WriteFile(path, []byte("not json{{{"), 0o644)
	pt := LoadPolicy(dir)
	if len(pt.penalties) != 0 {
		t.Error("corrupt policy file should return empty table")
	}
}

func TestIntentFromTask(t *testing.T) {
	cases := []struct{ task, want string }{
		{"implement new feature", "generate"},
		{"write unit tests for parser", "test"},
		{"fix the crash in handler", "debug"},
		{"refactor the storage layer", "refactor"},
		{"review auth middleware", "review"},
		{"find where error is logged", "search"},
		{"read the config file", "read"},
	}
	for _, c := range cases {
		if got := IntentFromTask(c.task); got != c.want {
			t.Errorf("IntentFromTask(%q) = %q, want %q", c.task, got, c.want)
		}
	}
}
