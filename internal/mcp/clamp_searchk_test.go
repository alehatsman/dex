package mcp

import (
	"strings"
	"testing"
)

// TestClampSearchK_Hint is the #543 regression: an overridden k must be
// reported in kHint (parity with the CLI's strict validation), while an
// omitted or in-range k stays silent.
func TestClampSearchK_Hint(t *testing.T) {
	root := t.TempDir() // no profile → no budget clamp interferes

	tests := []struct {
		name    string
		k       int
		wantK   int
		hintSub string // "" means no hint expected
	}{
		{"omitted defaults silently", 0, 8, ""},
		{"negative is invalid", -5, 8, "invalid"},
		{"over max is capped", 500, 30, "capped"},
		{"in range unchanged", 15, 15, ""},
		{"exactly max unchanged", 30, 30, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _, kHint := clampSearchK(SearchInput{K: tt.k}, root)
			if k != tt.wantK {
				t.Errorf("k = %d, want %d", k, tt.wantK)
			}
			if tt.hintSub == "" {
				if kHint != "" {
					t.Errorf("expected no hint, got %q", kHint)
				}
				return
			}
			if !strings.Contains(kHint, tt.hintSub) {
				t.Errorf("hint %q does not mention %q", kHint, tt.hintSub)
			}
		})
	}
}
