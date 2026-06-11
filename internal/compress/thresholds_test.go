package compress

import (
	"strings"
	"testing"
)

func TestThresholdsFor(t *testing.T) {
	tests := []struct {
		ext       string
		wantBPE   float64
		wantDelta float64
	}{
		{"go", 0.90, 0.55},
		{".go", 0.90, 0.55}, // leading dot stripped
		{"GO", 0.90, 0.55},  // case-insensitive
		{"py", 1.20, 0.55},
		{"json", 0.60, 0.40},
		{"yaml", 0.70, 0.45},
		{"yml", 0.70, 0.45},
		{"ts", 0.95, 0.58},
		{"tsx", 0.95, 0.58},
		{"java", 0.80, 0.50},
		{"rb", 1.15, 0.55},
		{"unknown", 0.85, 0.55}, // default
		{"", 0.85, 0.55},        // empty → default
	}
	for _, tc := range tests {
		th := thresholdsFor(tc.ext)
		if th.minEntropyBitsPerChar != tc.wantBPE {
			t.Errorf("thresholdsFor(%q).minEntropyBitsPerChar = %v, want %v", tc.ext, th.minEntropyBitsPerChar, tc.wantBPE)
		}
		if th.autoDelta != tc.wantDelta {
			t.Errorf("thresholdsFor(%q).autoDelta = %v, want %v", tc.ext, th.autoDelta, tc.wantDelta)
		}
	}
}

func TestEntropyFilterThreshold(t *testing.T) {
	// JSON (0.60) → EntropyThresholdLite (2.5): most aggressive
	json := thresholdsFor("json")
	if got := json.entropyFilterThreshold(); got != EntropyThresholdLite {
		t.Errorf("json.entropyFilterThreshold() = %.4f, want %.4f", got, EntropyThresholdLite)
	}

	// Python (1.20) → EntropyThresholdMax (3.5): least aggressive
	py := thresholdsFor("py")
	if got := py.entropyFilterThreshold(); got != EntropyThresholdMax {
		t.Errorf("py.entropyFilterThreshold() = %.4f, want %.4f", got, EntropyThresholdMax)
	}

	// Go (0.90) → midpoint ~3.0 (standard)
	goTh := thresholdsFor("go")
	if got := goTh.entropyFilterThreshold(); got < 2.9 || got > 3.1 {
		t.Errorf("go.entropyFilterThreshold() = %.4f, want near 3.0", got)
	}

	// Dense > verbose ordering
	rb := thresholdsFor("rb")
	java := thresholdsFor("java")
	if rb.entropyFilterThreshold() <= java.entropyFilterThreshold() {
		t.Errorf("Ruby threshold %.4f should exceed Java %.4f (Ruby is denser)",
			rb.entropyFilterThreshold(), java.entropyFilterThreshold())
	}
}

func TestDropLowEntropyLines(t *testing.T) {
	t.Run("blank lines always kept", func(t *testing.T) {
		lines := []string{"", "   ", "func foo() {", ""}
		out := dropLowEntropyLines(lines, 99.0) // impossibly high threshold
		blanks := 0
		for _, l := range out {
			if strings.TrimSpace(l) == "" {
				blanks++
			}
		}
		if blanks != 3 {
			t.Errorf("expected 3 blank lines preserved, got %d", blanks)
		}
	})

	t.Run("high-entropy lines kept at standard threshold", func(t *testing.T) {
		lines := []string{
			"func parseIPv4Address(s string) (net.IP, error) {",
			"return nil, fmt.Errorf(\"invalid address: %w\", err)",
		}
		out := dropLowEntropyLines(lines, EntropyThresholdStandard)
		if len(out) == 0 {
			t.Error("expected high-entropy code lines to survive standard threshold")
		}
	})

	t.Run("very low threshold keeps everything", func(t *testing.T) {
		lines := []string{"x := 1", "y := 2", "z := 3"}
		out := dropLowEntropyLines(lines, 0.0)
		if len(out) != len(lines) {
			t.Errorf("threshold 0.0: got %d lines, want %d", len(out), len(lines))
		}
	})
}

func TestAggressiveCompressLangThresholds(t *testing.T) {
	// Build a file large enough to trigger all passes (>200 lines).
	var sb strings.Builder
	for i := 0; i < 250; i++ {
		sb.WriteString("    x := someFunction(arg1, arg2, arg3)\n")
	}
	content := sb.String()

	// Both should compress, but Python's higher threshold keeps more lines.
	goResult := AggressiveCompress(content, ".go")
	pyResult := AggressiveCompress(content, ".py")

	// Python threshold is higher → less aggressive → more lines kept.
	// Go threshold is lower → more lines dropped.
	goLines := strings.Count(goResult, "\n")
	pyLines := strings.Count(pyResult, "\n")
	if goLines > pyLines {
		t.Errorf("Go (%d lines) should be <= Python (%d lines) after aggressive compress", goLines, pyLines)
	}
}
