package mcp

import (
	"testing"

	"github.com/alehatsman/dex/internal/heatmap"
)

func TestTopFilesByNetTokens(t *testing.T) {
	hm := heatmap.Load(t.TempDir()) // empty (no file) → initialized empty map
	// Force entries via RecordAccess so we exercise the real API.
	hm.RecordAccess("a.go", 1000, 100) // net 900
	hm.RecordAccess("b.go", 5000, 500) // net 4500
	hm.RecordAccess("c.go", 200, 0)    // net 200

	got := topFilesByNetTokens(hm, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].Path != "b.go" {
		t.Errorf("rank[0]: want b.go, got %q (net=%d)", got[0].Path, netTokens(got[0]))
	}
	if got[1].Path != "a.go" {
		t.Errorf("rank[1]: want a.go, got %q (net=%d)", got[1].Path, netTokens(got[1]))
	}
}

func TestBudgetAdvice(t *testing.T) {
	// Empty: silent.
	if got := budgetAdvice(BudgetOutput{}); got != "" {
		t.Errorf("empty out: want silent, got %q", got)
	}
	// Big file dominates.
	out := BudgetOutput{TopFiles: []BudgetFile{{Path: "huge.go", NetTokens: 50_000}}}
	if got := budgetAdvice(out); got == "" {
		t.Error("dominating file: want hint, got silent")
	}
	// Block violation overrides file hint.
	out = BudgetOutput{
		TopFiles:   []BudgetFile{{Path: "huge.go", NetTokens: 50_000}},
		Violations: []BudgetViolation{{Name: "ctx", Action: "block"}},
	}
	if got := budgetAdvice(out); got == "" || got[:3] != "SLO" {
		t.Errorf("block violation: want SLO hint, got %q", got)
	}
}
