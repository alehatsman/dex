package tokens

import (
	"math"
	"testing"
)

func TestCountEmpty(t *testing.T) {
	if Count("") != 0 {
		t.Error("empty string must be 0 tokens")
	}
}

func TestCountRealBPE(t *testing.T) {
	// o200k_base is exact and stable; these counts pin the real tokenizer.
	cases := []struct {
		s    string
		want int
	}{
		{"hello", 1},
		{"handleRequest", 2},  // merged camelCase pair, not 4 chars/4
		{"get_user_by_id", 4}, // snake_case splits on underscores
		{"α1", 2},             // greek letter + digit are separate tokens
	}
	for _, c := range cases {
		if got := Count(c.s); got != c.want {
			t.Errorf("Count(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestCountBeatsWordHeuristic(t *testing.T) {
	// The whole point of #152: real BPE diverges from the word-count guess.
	s := "func parseConfig(path string) (*Config, error) { return nil, nil }"
	words := 11
	if got := Count(s); got == words {
		t.Errorf("real BPE count (%d) should differ from word count (%d)", got, words)
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name string
		want Family
	}{
		{"claude-opus-4", Cl100k},
		{"anthropic.claude-sonnet", Cl100k},
		{"gemini-2.0-flash", Gemini},
		{"google/gemini-pro", Gemini},
		{"qwen2.5-coder", Llama},
		{"deepseek-v3", Llama},
		{"llama-3.3-70b", Llama},
		{"gpt-4o", O200kBase},
		{"", O200kBase},
	}
	for _, c := range cases {
		if got := Detect(c.name); got != c.want {
			t.Errorf("Detect(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFamilyString(t *testing.T) {
	cases := map[Family]string{
		O200kBase: "o200k_base",
		Cl100k:    "cl100k_base",
		Gemini:    "gemini",
		Llama:     "llama",
	}
	for f, want := range cases {
		if got := f.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", f, got, want)
		}
	}
}

func TestGeminiCorrection(t *testing.T) {
	// Gemini applies a 1.08x ceiling correction over its o200k base count.
	s := "the quick brown fox jumps over the lazy dog and runs away quickly"
	base := CountFor(s, O200kBase)
	gem := CountFor(s, Gemini)
	if gem < base {
		t.Errorf("gemini count %d must be >= o200k base %d", gem, base)
	}
	if base >= 10 && gem == base {
		t.Errorf("gemini correction should raise the count for non-trivial input (base=%d)", base)
	}
}

func TestHeuristicGeminiCorrection(t *testing.T) {
	// The BPE-less fallback must stay family-honest: Gemini gets the same
	// 1.08x correction bpeCounter applies, so a broken embedded-ranks load
	// doesn't silently under-count Gemini (#183).
	s := "the quick brown fox jumps over the lazy dog and runs away quickly today"
	base := (&heuristicCounter{family: O200kBase}).Count(s)
	gem := (&heuristicCounter{family: Gemini}).Count(s)
	if gem <= base {
		t.Errorf("heuristic gemini count %d must exceed o200k base %d", gem, base)
	}
	if want := int(math.Ceil(float64(base) * geminiCorrection)); gem < base && gem != want {
		t.Errorf("heuristic gemini = %d, want ~%d (base %d * %.2f)", gem, want, base, geminiCorrection)
	}
}

func TestCacheConsistency(t *testing.T) {
	s := "repeated string for cache hit"
	first := Count(s)
	for i := 0; i < 5; i++ {
		if got := Count(s); got != first {
			t.Errorf("cached count drifted: %d != %d", got, first)
		}
	}
}

func TestSetDefaultFamily(t *testing.T) {
	// Restore the original default after the test so we don't pollute other tests.
	t.Cleanup(func() { SetDefaultFamily(DefaultFamily) })

	SetDefaultFamily(Cl100k)
	if got := def().Family(); got != Cl100k {
		t.Errorf("after SetDefaultFamily(Cl100k): def().Family() = %v, want %v", got, Cl100k)
	}

	// Counts should still work and be non-zero after a family switch.
	if n := Count("hello world"); n == 0 {
		t.Error("Count returned 0 after SetDefaultFamily")
	}

	// Switch back to O200kBase via SetDefaultFamily to confirm it takes effect.
	SetDefaultFamily(O200kBase)
	if got := def().Family(); got != O200kBase {
		t.Errorf("after SetDefaultFamily(O200kBase): def().Family() = %v, want %v", got, O200kBase)
	}
}
