package retrieve

import (
	"testing"
)

// TestPickSuggestedReads_AssembleSymbolFirst guards #699: when intent=assemble
// and the symbol lane has exact hits, their files must lead suggested_reads
// so the agent is not misdirected by unrelated semantic hits.
func TestPickSuggestedReads_AssembleSymbolFirst(t *testing.T) {
	syms := []SymbolHit{
		{Path: "internal/mcp/server_review.go", StartLine: 387, EndLine: 414, QualifiedName: "hunkRisk"},
		{Path: "internal/mcp/server_review.go", StartLine: 220, EndLine: 311, QualifiedName: "reviewFile"},
	}
	// Semantic hits return completely unrelated files (naming collision in eval).
	sem := []SemHit{
		{Path: "internal/eval/bootstrap.go", StartLine: 60, EndLine: 118, Score: 0.95, Kind: "function_declaration"},
		{Path: "internal/eval/bootstrap_test.go", StartLine: 5, EndLine: 20, Score: 0.90, Kind: "function_declaration"},
	}
	symbolPaths := map[string]struct{}{"internal/mcp/server_review.go": {}}

	reads := PickSuggestedReads(IntentAssemble, sem, syms, symbolPaths, nil, func(string) bool { return false })

	if len(reads) == 0 {
		t.Fatal("expected at least one suggested read")
	}
	if reads[0].Path != "internal/mcp/server_review.go" {
		t.Errorf("suggested_reads[0].Path = %q, want server_review.go — symbol-lane file must lead for assemble", reads[0].Path)
	}
}

// TestPickSuggestedReads_AssembleNoSymbols verifies that when there are no
// symbol hits, assemble falls through to the semantic lane (existing behavior).
func TestPickSuggestedReads_AssembleNoSymbols(t *testing.T) {
	sem := []SemHit{
		{Path: "internal/eval/bootstrap.go", StartLine: 60, EndLine: 118, Score: 0.95, Kind: "function_declaration"},
	}

	reads := PickSuggestedReads(IntentAssemble, sem, nil, nil, nil, func(string) bool { return false })

	if len(reads) == 0 {
		t.Fatal("expected at least one suggested read from semantic lane")
	}
	if reads[0].Path != "internal/eval/bootstrap.go" {
		t.Errorf("suggested_reads[0].Path = %q, want eval/bootstrap.go — semantic fallback when no symbols", reads[0].Path)
	}
}
