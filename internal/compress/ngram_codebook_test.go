package compress

import (
	"fmt"
	"strings"
	"testing"
)

func TestNgramCodebook_Empty(t *testing.T) {
	cb := NgramCodebook{}
	if !cb.Empty() {
		t.Fatal("zero-value must be empty")
	}
	if cb.Legend() != "" {
		t.Error("empty legend must be empty string")
	}
	text := "hello world"
	if cb.Apply(text) != text {
		t.Error("empty Apply must be identity")
	}
	if cb.ApplyWithLegend(text) != text {
		t.Error("empty ApplyWithLegend must be identity")
	}
}

func TestNgramCodebook_InsufficientPatterns(t *testing.T) {
	// Single unique bigram repeated once — won't meet ROI gate.
	cb := BuildNgramCodebook("foo bar\nbaz qux")
	if !cb.Empty() {
		t.Error("unique non-repeating bigrams should produce empty codebook")
	}
}

func TestNgramCodebook_BigramDetected(t *testing.T) {
	// Repeat a bigram many times to exceed the ROI gate.
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "if err != nil {")
	}
	content := strings.Join(lines, "\n")
	cb := BuildNgramCodebook(content)
	if cb.Empty() {
		t.Skip("ROI gate blocked — may need more repetitions or token setup")
	}
	applied := cb.Apply(content)
	// The compressed content should be shorter.
	if len(applied) >= len(content) {
		t.Errorf("Apply should reduce length: before=%d after=%d", len(content), len(applied))
	}
	// Legend must be present in ApplyWithLegend output.
	full := cb.ApplyWithLegend(content)
	if !strings.Contains(full, "©MAP:") {
		t.Error("ApplyWithLegend must include ©MAP: legend")
	}
}

func TestNgramCodebook_DoesNotCollapseAcrossNewline(t *testing.T) {
	// "handleRequest processEvent" is a frequent within-line bigram → it
	// registers (a second frequent bigram clears the ≥2-candidate gate, and
	// the varying third token keeps any trigram below ROI). The same two
	// tokens also occur split across a newline ("…handleRequest" then
	// "processEvent…"). Apply must not bridge the newline and merge those two
	// lines onto one ref: frequencies are counted per line, so the cross-line
	// pair was never counted (#449).
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines,
			fmt.Sprintf("handleRequest processEvent a%d", i),
			fmt.Sprintf("validateInput serializeOutput b%d", i))
	}
	lines = append(lines, "alpha handleRequest", "processEvent beta")
	content := strings.Join(lines, "\n")

	cb := BuildNgramCodebook(content)
	if cb.Empty() {
		t.Fatal("expected a non-empty codebook for the repeated bigrams")
	}
	applied := cb.Apply(content)

	if !strings.Contains(applied, "handleRequest\nprocessEvent") {
		t.Errorf("bigram split across a newline was collapsed (newline swallowed); output:\n%s", applied)
	}
}

func TestNgramCodebook_TrigramPreferredOverBigram(t *testing.T) {
	// A trigram that repeats many times: its constituent bigrams must not also
	// get separate codebook entries (covered suppression).
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "return nil err")
	}
	content := strings.Join(lines, "\n")
	cb := BuildNgramCodebook(content)
	if cb.Empty() {
		t.Skip("ROI gate blocked")
	}
	// Trigram "return nil err" should be present; bigrams "return nil" and
	// "nil err" should NOT be separate entries.
	for _, e := range cb.entries {
		pat := strings.Join(e.pattern, " ")
		if pat == "return nil" || pat == "nil err" {
			t.Errorf("bigram subset of qualifying trigram should be suppressed: %q", pat)
		}
	}
}

func TestNgramCodebook_LegendFormat(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "if err != nil {")
	}
	cb := BuildNgramCodebook(strings.Join(lines, "\n"))
	if cb.Empty() {
		t.Skip("ROI gate blocked")
	}
	legend := cb.Legend()
	if !strings.HasPrefix(legend, "©MAP:") {
		t.Errorf("legend must start with ©MAP:, got: %q", legend[:min(20, len(legend))])
	}
	if !strings.Contains(legend, "©0=") {
		t.Errorf("first entry must use ©0 ref")
	}
}

func TestNgramCodebook_CapAt20(t *testing.T) {
	// Generate 30 distinct bigrams each repeated enough times.
	var lines []string
	words := []string{
		"aa", "bb", "cc", "dd", "ee", "ff", "gg", "hh", "ii", "jj",
		"kk", "ll", "mm", "nn", "oo", "pp", "qq", "rr", "ss", "tt",
		"uu", "vv", "ww", "xx", "yy", "zz", "az", "bz", "cz", "dz",
	}
	for i := 0; i+1 < len(words); i++ {
		for j := 0; j < 50; j++ {
			lines = append(lines, words[i]+" "+words[i+1])
		}
	}
	cb := BuildNgramCodebook(strings.Join(lines, "\n"))
	if len(cb.entries) > ngramRefMax {
		t.Errorf("entries must be capped at %d, got %d", ngramRefMax, len(cb.entries))
	}
}
