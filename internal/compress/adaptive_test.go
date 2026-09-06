package compress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

	// Saturate the penalty for aggressive directly — RecordFeedback (the write
	// path that used to populate this) was removed as dead code in #856;
	// nothing calls it, so ChooseMode only ever sees whatever penalties were
	// already on disk. Seed the map directly to keep testing that behavior.
	pt.penalties["generate"] = map[string]float64{"aggressive": 1.0}

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
	// Write the policy file directly (the save() write path was removed as
	// dead code in #856 alongside RecordFeedback) to exercise the load side.
	pj := policyJSON{Penalties: map[string]map[string]float64{"refactor": {"aggressive": 0.5}}}
	data, err := json.Marshal(pj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, policyFile), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	pt := LoadPolicy(dir)
	if pt.penalties["refactor"]["aggressive"] != 0.5 {
		t.Errorf("persisted penalty mismatch: want 0.5 got %f", pt.penalties["refactor"]["aggressive"])
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
