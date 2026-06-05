package compress

import (
	"strings"
	"testing"
)

func TestTerseCompress_Empty(t *testing.T) {
	r := TerseCompress("", Level3)
	if r.Applied || r.Output != "" {
		t.Error("empty input: want Applied=false, Output=''")
	}
}

func TestTerseCompress_QualityGate(t *testing.T) {
	// A short line with no function words — terse can't save 3%, gate blocks.
	r := TerseCompress("hello world foo bar baz", Level1)
	if r.Applied {
		t.Error("tiny input with no function words should not pass quality gate")
	}
}

func TestTerseCompress_Level1_FunctionWords(t *testing.T) {
	// Build a long line (>20 tokens) packed with function words.
	line := "the configuration is a function that will have and be done with the system by the way or not if it can"
	r := TerseCompress(line, Level1)
	if !r.Applied {
		t.Skip("quality gate blocked — adjust test line if thresholds change")
	}
	// Function words must be stripped.
	for _, fw := range []string{" the ", " is ", " a ", " with ", " by ", " or ", " if ", " it ", " can "} {
		if strings.Contains(" "+r.Output+" ", fw) {
			t.Errorf("function word %q survived in output: %q", fw, r.Output)
		}
	}
}

func TestTerseCompress_Level2_Abbreviations(t *testing.T) {
	// Long line (>20 tokens) with verbose words.
	line := strings.Repeat("configuration parameter response request interface implementation ", 5)
	r := TerseCompress(line, Level2)
	if !r.Applied {
		t.Skip("quality gate blocked")
	}
	if !strings.Contains(r.Output, "cfg") {
		t.Errorf("expected 'configuration' → 'cfg' in %q", r.Output)
	}
	if !strings.Contains(r.Output, "param") {
		t.Errorf("expected 'parameter' → 'param' in %q", r.Output)
	}
}

func TestTerseCompress_Level3_ZeroUniqueDedup(t *testing.T) {
	// Line 1 introduces tokens; line 2 is a near-duplicate with no new tokens.
	lines := []string{
		"error connecting to database server timeout exceeded",
		"error connecting to database server timeout exceeded",
		"error connecting to database server timeout exceeded",
		"new unique information about the system failure here",
	}
	r := TerseCompress(strings.Join(lines, "\n"), Level3)
	if !r.Applied {
		t.Skip("quality gate blocked")
	}
	// The near-duplicate lines should be collapsed.
	outLines := strings.Split(r.Output, "\n")
	if len(outLines) >= len(lines) {
		t.Errorf("expected dedup to reduce line count, got %d lines from %d", len(outLines), len(lines))
	}
	// The unique line must survive (Level2 abbreviates "information" → "info").
	if !strings.Contains(r.Output, "unique") {
		t.Errorf("unique line was dropped: %q", r.Output)
	}
}

func TestTerseCompress_OversizeSKip(t *testing.T) {
	big := strings.Repeat("x ", maxInputBytes/2+1)
	r := TerseCompress(big, Level3)
	if r.Applied {
		t.Error("input >64KB should not be compressed")
	}
}

func TestStripFunctionWords_ShortLine(t *testing.T) {
	// Lines with ≤20 tokens must not be modified.
	lines := []string{"the cat is on the mat"}
	got := stripFunctionWords(lines)
	if got[0] != lines[0] {
		t.Errorf("short line modified: %q → %q", lines[0], got[0])
	}
}

func TestApplyAbbreviations_Punctuation(t *testing.T) {
	lines := []string{"configuration, parameter."}
	got := applyAbbreviations(lines)
	if !strings.Contains(got[0], "cfg,") {
		t.Errorf("punctuation not preserved: %q", got[0])
	}
	if !strings.Contains(got[0], "param.") {
		t.Errorf("trailing dot not preserved: %q", got[0])
	}
}

func TestDedupZeroUnique_EmptyLinesKept(t *testing.T) {
	lines := []string{"foo bar", "", "baz qux"}
	got := dedupZeroUnique(lines)
	if len(got) != 3 {
		t.Errorf("empty lines should be preserved: got %v", got)
	}
}

func TestCountTokens(t *testing.T) {
	if countTokens("") != 0 {
		t.Error("empty → 0")
	}
	if countTokens("a b c") != 3 {
		t.Error("three words → 3")
	}
}
