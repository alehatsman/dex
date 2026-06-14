package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/store"
)

func TestBuildFaithfulnessEvidence(t *testing.T) {
	hits := []store.Hit{
		{Path: "a.go", StartLine: 1, EndLine: 3, Content: "func A() {}"},
		{Path: "b.go", StartLine: 10, EndLine: 12, Content: "func B() {}"},
	}
	ev := buildFaithfulnessEvidence(hits)
	for _, want := range []string{"a.go (L1-3)", "func A() {}", "b.go (L10-12)", "func B() {}"} {
		if !strings.Contains(ev, want) {
			t.Errorf("evidence missing %q:\n%s", want, ev)
		}
	}
}

func TestBuildFaithfulnessEvidence_Capped(t *testing.T) {
	big := strings.Repeat("x", faithfulnessEvidenceCap*2)
	ev := buildFaithfulnessEvidence([]store.Hit{{Path: "p", Content: big}})
	if len(ev) > faithfulnessEvidenceCap {
		t.Errorf("evidence not capped: got %d bytes, cap %d", len(ev), faithfulnessEvidenceCap)
	}
}

func TestCheckFaithfulnessRegression(t *testing.T) {
	dir := t.TempDir()
	ref := eval.FaithfulnessReport{MeanScore: 0.80}
	data, _ := json.Marshal(ref)
	refPath := filepath.Join(dir, "ref.json")
	if err := os.WriteFile(refPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Within tolerance (and above) → pass.
	if err := checkFaithfulnessRegression(eval.FaithfulnessReport{MeanScore: 0.79}, refPath); err != nil {
		t.Errorf("0.79 vs 0.80 (tol 0.02) should pass, got %v", err)
	}
	if err := checkFaithfulnessRegression(eval.FaithfulnessReport{MeanScore: 0.95}, refPath); err != nil {
		t.Errorf("improvement should pass, got %v", err)
	}
	// Beyond tolerance → fail.
	if err := checkFaithfulnessRegression(eval.FaithfulnessReport{MeanScore: 0.70}, refPath); err == nil {
		t.Error("0.70 vs 0.80 should fail the regression check")
	}
}
