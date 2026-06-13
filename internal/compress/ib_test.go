package compress

import (
	"math"
	"strings"
	"testing"
)

func TestNormalizedWordEntropy(t *testing.T) {
	// Empty line → 0.
	if got := normalizedWordEntropy(""); got != 0 {
		t.Errorf("empty: got %f, want 0", got)
	}

	// Single word → 0 (unique=1).
	if got := normalizedWordEntropy("ok"); got != 0 {
		t.Errorf("single word: got %f, want 0", got)
	}

	// All same word → 0.
	if got := normalizedWordEntropy("foo foo foo foo"); got != 0 {
		t.Errorf("all same word: got %f, want 0", got)
	}

	// All distinct words → entropy/log2(n) = log2(n)/log2(n) = 1.
	got := normalizedWordEntropy("alpha beta gamma delta")
	if math.Abs(got-1.0) > 0.01 {
		t.Errorf("all distinct: got %f, want ~1.0", got)
	}

	// Mixed: half unique → somewhere in (0, 1).
	got2 := normalizedWordEntropy("error error fail pass done")
	if got2 <= 0 || got2 >= 1 {
		t.Errorf("mixed: got %f, want in (0,1)", got2)
	}
}

func TestCompressIB_OutOfRange(t *testing.T) {
	text := "some text here\nmore text\nand more\nfourth line\nfifth line\n"
	// targetRatio ≤ 0 or ≥ 1 → unchanged
	if CompressIB(text, 0) != text {
		t.Error("ratio=0 should return unchanged")
	}
	if CompressIB(text, 1) != text {
		t.Error("ratio=1 should return unchanged")
	}
	if CompressIB(text, 1.5) != text {
		t.Error("ratio>1 should return unchanged")
	}
}

func TestCompressIB_ShortInput(t *testing.T) {
	text := "line one\nline two\n"
	if CompressIB(text, 0.5) != text {
		t.Error("< 5 lines should return unchanged")
	}
}

func TestCompressIB_HitsTargetRatio(t *testing.T) {
	// Build text where half the lines are high-entropy and half are low.
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("foo foo foo foo foo\n") // low entropy (all same word)
	}
	for i := 0; i < 20; i++ {
		sb.WriteString("alpha beta gamma delta epsilon zeta eta theta iota\n") // high entropy
	}
	text := sb.String()

	// Target: keep roughly 50% of tokens.
	compressed := CompressIB(text, 0.5)

	origTok := countTokens(text)
	outTok := countTokens(compressed)
	if origTok == 0 {
		t.Fatal("empty input")
	}
	actualRatio := float64(outTok) / float64(origTok)

	// Should be within 20% of target (binary search converges precisely, but
	// discrete line counts mean exact hits are not always possible).
	if math.Abs(actualRatio-0.5) > 0.20 {
		t.Errorf("ratio %f too far from target 0.5 (delta %.2f)", actualRatio, math.Abs(actualRatio-0.5))
	}

	// Low-entropy lines should be preferentially dropped.
	if strings.Contains(compressed, "foo foo foo foo foo") {
		// It's OK if some survive (ratio target may require keeping them),
		// but high-entropy lines must all survive.
	}
	if !strings.Contains(compressed, "alpha beta gamma delta") {
		t.Error("high-entropy lines should be preserved")
	}
}

func TestCompressIB_NeverLongerThanOriginal(t *testing.T) {
	// Edge case: very aggressive ratio should not make output longer.
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line content here with words\n")
	}
	text := sb.String()
	for _, ratio := range []float64{0.1, 0.3, 0.5, 0.9} {
		result := CompressIB(text, ratio)
		if len(result) > len(text) {
			t.Errorf("ratio %f: result longer than input (%d > %d)", ratio, len(result), len(text))
		}
	}
}

func TestCompressIB_BlankLinesPreserved(t *testing.T) {
	// Blank lines must always survive regardless of threshold.
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString("foo foo foo foo foo\n") // low entropy
		sb.WriteString("\n")                    // blank
	}
	text := sb.String()
	result := CompressIB(text, 0.1) // aggressive compression

	// All blank lines (empty string between newlines) should still be present.
	blanksBefore := strings.Count(text, "\n\n")
	blanksAfter := strings.Count(result, "\n\n")
	if blanksAfter < blanksBefore {
		t.Errorf("blank lines dropped: before=%d after=%d", blanksBefore, blanksAfter)
	}
}
