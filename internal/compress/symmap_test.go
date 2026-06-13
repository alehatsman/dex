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
	// Three long snake_case identifiers (each ≥2 real BPE tokens) appearing
	// many times. Refs are 1-token single chars, so identifiers of ≥2 tokens
	// clear the gate — these do.
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
	// At least one single-token ref must appear in the output.
	found := false
	for _, ref := range singleTokenRefs {
		if strings.Contains(applied, ref) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one single-token ref in applied output")
	}
}

func TestBuildSymbolMap_LongestFirst(t *testing.T) {
	// "get_user_by_id_cached" contains "get_user_by_id" — the longer ident must
	// be replaced first. A third long ident is included to clear the ≥3 gate.
	// All three are ≥2 real BPE tokens, so they survive honest ROI gating.
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

func TestApply_WholeWordOnly(t *testing.T) {
	// #447: parseConfig is frequent and registers; parseConfigFile appears
	// once (below ROI) so it is NOT registered. The registered parseConfig
	// embedded inside the longer parseConfigFile must NOT be rewritten — a
	// substring replace would corrupt it into "<ref>File". This keeps Apply
	// in step with the ROI model, which counts parseConfigFile as its own
	// token, never as a parseConfig occurrence.
	var sb strings.Builder
	for i := 0; i < 12; i++ {
		sb.WriteString("parseConfig\nhandleRequest\nvalidateInput\n")
	}
	sb.WriteString("parseConfigFile\n") // single occurrence → unregistered
	content := sb.String()

	sm := BuildSymbolMap(content)
	if sm.Empty() {
		t.Fatal("expected non-empty map for three frequent identifiers")
	}
	applied := sm.Apply(content)

	// The unregistered longer identifier must survive whole and intact.
	if !strings.Contains(applied, "parseConfigFile") {
		t.Errorf("parseConfigFile (unregistered) was mangled by substring replace; output:\n%s", applied)
	}
	// Standalone parseConfig occurrences must still be replaced.
	if strings.Contains(applied, "\nparseConfig\n") {
		t.Error("standalone parseConfig should have been replaced with its ref")
	}
}

func TestSymROI_ShortIdent(t *testing.T) {
	if _, ok := symROI("short", 100); ok {
		t.Error("identifier shorter than 6 chars must not register")
	}
}

func TestSymROI_NoSaving(t *testing.T) {
	// Real BPE: symTokens("abcdefgh")=1, refToks=1 → savingPer=0 ≤ 0,
	// so the substitution can never pay for itself regardless of occurrences.
	if _, ok := symROI("abcdefgh", 100); ok {
		t.Error("1-token identifier cannot save with a 1-token ref")
	}
}

func TestSymROI_Profitable(t *testing.T) {
	// Real BPE: symTokens("get_user_by_id")=4, refToks=1 → savingPer=3;
	// entryCost = 4+1+2 = 7; occurrences=5 → totalSavings=15, net=8 > 0 ✓.
	if _, ok := symROI("get_user_by_id", 5); !ok {
		t.Error("profitable identifier should register")
	}
}

func TestSymROI_MarginalProfitable(t *testing.T) {
	// handleRequest = 2 BPE tokens, refToks=1, savingPer=1.
	// With 1-token refs, 2-token identifiers now qualify (they couldn't with αN=2).
	// occurrences=6: totalSavings=6, entryCost=2+1+2=5, net=1 > 0 ✓.
	if _, ok := symROI("handleRequest", 6); !ok {
		t.Error("2-token identifier with enough occurrences should register under 1-token ref scheme")
	}
}

func TestSymROI_MarginalNotProfitable(t *testing.T) {
	// handleRequest = 2 BPE tokens, refToks=1, savingPer=1.
	// occurrences=4: totalSavings=4, entryCost=2+1+2=5, net=-1 ≤ 0.
	if _, ok := symROI("handleRequest", 4); ok {
		t.Error("2-token identifier with too few occurrences must not register")
	}
}

func TestApplyWithLegend_Format(t *testing.T) {
	// With 1-token refs, 2-token identifiers (handleRequest, parseResponse,
	// validateInput) now qualify — previously these were blocked by the αN=2 gate.
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
		t.Fatal("2-token identifiers with ≥6 occurrences should qualify under 1-token ref scheme")
	}
	out := sm.ApplyWithLegend(sb.String())
	if !strings.HasPrefix(out, "§MAP:\n") {
		t.Errorf("ApplyWithLegend must start with §MAP:\\n, got: %q", out[:20])
	}
}

func TestSymTokens(t *testing.T) {
	// Real BPE token counts (o200k_base, the default counting family).
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
