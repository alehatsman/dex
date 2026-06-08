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
	// A single genuinely-profitable identifier (get_user_by_id = 4 BPE tokens)
	// repeated many times — qualifies on its own ROI but is below the ≥3
	// distinct-identifier threshold, so no legend is emitted.
	content := strings.Repeat("get_user_by_id\n", 20)
	sm := BuildSymbolMap(content)
	if !sm.Empty() {
		t.Error("expected empty map: fewer than 3 qualifying identifiers")
	}
}

func TestBuildSymbolMap_ROIGate(t *testing.T) {
	// Three long snake_case identifiers (each ≥3 real BPE tokens) appearing
	// many times. Under honest tokenization the α-ref itself costs 2 tokens,
	// so only identifiers of ≥3 tokens clear the gate — these do.
	idents := []string{"get_user_by_id", "parse_http_header", "validate_user_input"}
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
	// "get_user_by_id_cached" contains "get_user_by_id" — the longer ident must
	// be replaced first. A third long ident is included to clear the ≥3 gate.
	// All three are ≥3 real BPE tokens, so they survive honest ROI gating.
	content := strings.Repeat("get_user_by_id_cached\nget_user_by_id\nparse_http_header\n", 10)
	sm := BuildSymbolMap(content)
	if sm.Empty() {
		t.Fatal("expected non-empty map for three profitable identifiers")
	}
	applied := sm.Apply(content)
	// Neither the long identifier nor its prefix should survive in the output.
	if strings.Contains(applied, "get_user_by_id_cached") || strings.Contains(applied, "get_user_by_id") {
		t.Error("identifiers not fully replaced")
	}
}

func TestShouldRegisterSym_ShortIdent(t *testing.T) {
	if shouldRegisterSym("short", 100, 1) {
		t.Error("identifier shorter than 6 chars must not register")
	}
}

func TestShouldRegisterSym_NoSaving(t *testing.T) {
	// Real BPE: symTokens("sixchr")=2, symTokens("α1")=2 → savingPer=0 ≤ 0,
	// so the substitution can never pay for itself regardless of occurrences.
	if shouldRegisterSym("sixchr", 1, 1) {
		t.Error("identifier no larger than its ref should not register")
	}
}

func TestShouldRegisterSym_Profitable(t *testing.T) {
	// Real BPE: symTokens("get_user_by_id")=4, symTokens("α1")=2 → savingPer=2;
	// entryCost = 4+2+2 = 8; occurrences=5 → totalSavings=10 > 8 ✓.
	if !shouldRegisterSym("get_user_by_id", 5, 1) {
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
	// Real BPE token counts (o200k_base, the default counting family). These
	// differ from the old rune/4 heuristic — e.g. "handleRequest" is one merged
	// camelCase pair (2 tokens), not 4, and the α-ref costs 2, not 1.
	cases := []struct {
		s    string
		want int
	}{
		{"abcd", 1},
		{"abcdefgh", 1},
		{"handleRequest", 2},
		{"get_user_by_id", 4},
		{"α1", 2},
	}
	for _, c := range cases {
		if got := symTokens(c.s); got != c.want {
			t.Errorf("symTokens(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}
