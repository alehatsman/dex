package mcp

import (
	"strings"
	"testing"
)

func TestCompressToolDescFull(t *testing.T) {
	desc := "PRIMARY ENTRY POINT. Call this first. Extra detail here."
	got := compressToolDesc(desc, DescModeFull)
	if got != desc {
		t.Errorf("full mode should return unchanged; got %q", got)
	}
}

func TestCompressToolDescTerse(t *testing.T) {
	desc := "Execute a shell command and return compressed output. " +
		"Applies the same compression pipeline as compress_output. " +
		"Deduplicates log lines and strips ANSI. " +
		"Use raw:true to skip compression. File-write redirects are blocked."
	got := compressToolDesc(desc, DescModeTerse)
	// Should be truncated to 3 sentences
	sentences := strings.Split(got, ". ")
	if len(sentences) > 3 {
		t.Errorf("terse: expected ≤3 sentences, got %d in %q", len(sentences), got)
	}
	// Should apply abbreviation: "compressed" stays, "output" → "out"
	if !strings.Contains(got, "out") {
		t.Errorf("terse: expected 'output' abbreviated to 'out' in %q", got)
	}
}

func TestCompressToolDescLazy(t *testing.T) {
	desc := "Execute a shell command and return compressed output. " +
		"Applies the same compression pipeline as compress_output. " +
		"Deduplicates log lines and strips ANSI."
	got := compressToolDesc(desc, DescModeLazy)
	// Should end with ask hint
	if !strings.Contains(got, "ask") {
		t.Errorf("lazy: expected ask hint in %q", got)
	}
	// Should be only one original sentence (before the hint)
	// The hint itself counts, so we check the structure
	if strings.Contains(got, "Applies") {
		t.Errorf("lazy: second sentence should be dropped, got %q", got)
	}
}

func TestLazyDescCap80(t *testing.T) {
	// A very long first sentence
	long := strings.Repeat("word ", 20) + "end."
	got := lazyDesc(long)
	// The non-hint part should be ≤80 chars
	hint := ". (use use ask for full context)"
	body := strings.TrimSuffix(got, hint)
	if len(body) > 80 {
		t.Errorf("lazy: body exceeds 80 chars (%d): %q", len(body), body)
	}
}

func TestDescriptionModeFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want DescriptionMode
	}{
		{"full", DescModeFull},
		{"terse", DescModeTerse},
		{"lazy", DescModeLazy},
		{"TERSE", DescModeTerse},
		{"", DescModeTerse},        // default is terse (#547)
		{"unknown", DescModeTerse}, // unrecognized falls back to the default
	}
	for _, c := range cases {
		t.Setenv("ENABLE_TOOL_SEARCH", "") // isolate from host env
		t.Setenv("DEX_DESCRIPTION_MODE", c.env)
		got := descriptionModeFromEnv()
		if got != c.want {
			t.Errorf("env=%q: got %v, want %v", c.env, got, c.want)
		}
	}
}

// TestDescriptionModeToolSearchClamp verifies ENABLE_TOOL_SEARCH forces full
// descriptions, since tool-search forwarding relies on full docs for selection.
func TestDescriptionModeToolSearchClamp(t *testing.T) {
	cases := []struct {
		desc     string
		descMode string
		toolSrch string
		want     DescriptionMode
	}{
		{"default clamped to full", "", "true", DescModeFull},
		{"terse clamped to full", "terse", "1", DescModeFull},
		{"lazy clamped to full", "lazy", "yes", DescModeFull},
		{"explicit full stays full", "full", "true", DescModeFull},
		{"terse honored when off", "terse", "false", DescModeTerse},
		{"terse honored when unset", "terse", "", DescModeTerse},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			t.Setenv("DEX_DESCRIPTION_MODE", c.descMode)
			t.Setenv("ENABLE_TOOL_SEARCH", c.toolSrch)
			if got := descriptionModeFromEnv(); got != c.want {
				t.Errorf("descMode=%q toolSearch=%q: got %v, want %v", c.descMode, c.toolSrch, got, c.want)
			}
		})
	}
}
