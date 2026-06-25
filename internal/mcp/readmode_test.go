package mcp

import "testing"

func TestReadModeClassification(t *testing.T) {
	cases := []struct {
		m         ReadMode
		isLines   bool
		isLLM     bool
		complete  bool
		lossy     bool
		needsIdx  bool
		cacheable bool
		valid     bool
	}{
		{ReadModeFull, false, false, true, false, false, true, true},
		{ReadModeSignatures, false, false, false, true, true, true, true},
		{ReadModeSkeleton, false, false, false, true, true, true, true},
		{ReadModeMap, false, false, false, true, true, true, true},
		{ReadModeAggressive, false, false, false, true, false, true, true},
		{ReadModeSummary, false, true, true, true, false, false, true},
		{ReadModeHandle, false, false, false, false, false, true, true},
		{ReadMode("lines:1-50"), true, false, false, false, false, true, true},
		{ReadModeLines, false, false, false, false, false, false, false}, // bare "lines" stand-in: never dispatched
		{ReadMode("bogus"), false, false, false, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(string(c.m), func(t *testing.T) {
			if got := c.m.IsLines(); got != c.isLines {
				t.Errorf("IsLines: got %v want %v", got, c.isLines)
			}
			if got := c.m.IsLLM(); got != c.isLLM {
				t.Errorf("IsLLM: got %v want %v", got, c.isLLM)
			}
			if got := c.m.IsComplete(); got != c.complete {
				t.Errorf("IsComplete: got %v want %v", got, c.complete)
			}
			if got := c.m.IsLossySummary(); got != c.lossy {
				t.Errorf("IsLossySummary: got %v want %v", got, c.lossy)
			}
			if got := c.m.NeedsIndex(); got != c.needsIdx {
				t.Errorf("NeedsIndex: got %v want %v", got, c.needsIdx)
			}
			if got := c.m.IsCompressedCacheable(); got != c.cacheable {
				t.Errorf("IsCompressedCacheable: got %v want %v", got, c.cacheable)
			}
			if got := ValidReadMode(c.m); got != c.valid {
				t.Errorf("ValidReadMode: got %v want %v", got, c.valid)
			}
		})
	}
}

func TestParseReadMode(t *testing.T) {
	if _, ok := ParseReadMode(""); ok {
		t.Errorf("empty string should report ok=false")
	}
	if _, ok := ParseReadMode("   "); ok {
		t.Errorf("whitespace-only should report ok=false")
	}
	m, ok := ParseReadMode("  Full  ")
	if !ok || m != ReadModeFull {
		t.Errorf("normalize failed: got (%q, %v)", m, ok)
	}
	m, ok = ParseReadMode("LINES:1-3")
	if !ok || m != ReadMode("lines:1-3") {
		t.Errorf("lines prefix not preserved post-normalize: got (%q, %v)", m, ok)
	}
}
