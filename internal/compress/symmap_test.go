package compress

import (
	"strings"
	"testing"
)

func TestBuildSymbolMap_Empty(t *testing.T) {
	sm := SymbolMap{}
	if !sm.Empty() {
		t.Fatal("zero-value must be empty")
	}
	if sm.Legend() != "" {
		t.Error("empty legend must be empty string")
	}
	text := "hello world"
	if sm.Apply(text) != text {
		t.Error("empty Apply must be identity")
	}
	if sm.ApplyWithLegend(text) != text {
		t.Error("empty ApplyWithLegend must be identity")
	}
}

func TestBuildSymbolMap_MinSymbols(t *testing.T) {
	// Only one high-ROI identifier — below the ≥3 threshold.
	content := strings.Repeat("handleRequest\n", 20)
	sm := BuildSymbolMap(content)
	if !sm.Empty() {
		t.Error("expected empty map: fewer than 3 qualifying identifiers")
	}
}

func TestBuildSymbolMap_ROIGate(t *testing.T) {
	// Build content with three long identifiers each appearing many times.
	idents := []string{"handleRequest", "parseResponse", "validateInput"}
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		for _, id := range idents {
			sb.WriteString(id)
			sb.WriteByte('\n')
		}
	}
	content := sb.String()

	sm := BuildSymbolMap(content)
	if sm.Empty() {
		t.Fatal("expected non-empty symbol map for high-frequency identifiers")
	}

	legend := sm.Legend()
	if !strings.HasPrefix(legend, "§MAP:\n") {
		t.Errorf("unexpected legend prefix: %q", legend)
	}
	for _, id := range idents {
		if !strings.Contains(legend, id) {
			t.Errorf("identifier %q missing from legend", id)
		}
	}

	applied := sm.Apply(content)
	for _, id := range idents {
		if strings.Contains(applied, id) {
			t.Errorf("identifier %q not replaced in applied output", id)
		}
	}
	if !strings.Contains(applied, "α1") {
		t.Error("expected α1 ref in applied output")
	}
}

func TestBuildSymbolMap_LongestFirst(t *testing.T) {
	// "handleRequestError" contains "handleRequest" — longer must be replaced first.
	content := strings.Repeat("handleRequestError\nhandleRequest\n", 10)
	sm := BuildSymbolMap(content)
	if sm.Empty() {
		t.Skip("too few qualifying identifiers for this content")
	}
	applied := sm.Apply(content)
	// Neither original identifier should survive in the output.
	if strings.Contains(applied, "handleRequestError") || strings.Contains(applied, "handleRequest") {
		t.Error("identifiers not fully replaced")
	}
}

func TestShouldRegisterSym_ShortIdent(t *testing.T) {
	if shouldRegisterSym("short", 100, 1) {
		t.Error("identifier shorter than 6 chars must not register")
	}
}

func TestShouldRegisterSym_NoSaving(t *testing.T) {
	// 6-char identifier: symTokens("sixchr")=(6+3)/4=2; symTokens("α1")=(2+3)/4=1
	// savingPer=2-1=1; totalSavings=1*1=1; entryCost=2+1+2=5; 1>5 false
	if shouldRegisterSym("sixchr", 1, 1) {
		t.Error("single occurrence of short identifier should not register")
	}
}

func TestShouldRegisterSym_Profitable(t *testing.T) {
	// Long identifier with many occurrences should register.
	// "handleRequestError" = 18 chars → symTokens = (18+3)/4 = 5
	// α1 → symTokens = (2+3)/4 = 1; savingPer = 4; entryCost = 5+1+2=8
	// occurrences=3: totalSavings=12 > 8 ✓
	if !shouldRegisterSym("handleRequestError", 3, 1) {
		t.Error("profitable identifier should register")
	}
}

func TestApplyWithLegend_Format(t *testing.T) {
	idents := []string{"handleRequest", "parseResponse", "validateInput"}
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		for _, id := range idents {
			sb.WriteString(id)
			sb.WriteByte('\n')
		}
	}
	sm := BuildSymbolMap(sb.String())
	if sm.Empty() {
		t.Skip("no qualifying identifiers")
	}
	out := sm.ApplyWithLegend(sb.String())
	if !strings.HasPrefix(out, "§MAP:\n") {
		t.Errorf("ApplyWithLegend must start with §MAP:\\n, got: %q", out[:20])
	}
}

func TestSymTokens(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"abcd", 1},       // 4 runes → (4+3)/4 = 1
		{"abcdefgh", 2},   // 8 runes → (8+3)/4 = 2
		{"handleRequest", 4}, // 13 runes → (13+3)/4 = 4
		{"α1", 1},         // 2 runes → (2+3)/4 = 1
	}
	for _, c := range cases {
		if got := symTokens(c.s); got != c.want {
			t.Errorf("symTokens(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}
