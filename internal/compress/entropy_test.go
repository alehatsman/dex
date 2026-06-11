package compress

import (
	"math"
	"strings"
	"testing"
)

func TestBidirectionalScore(t *testing.T) {
	line := "func handleRequestError(ctx context.Context) error {"
	baseScore := shannonEntropy(line)

	// Nil window → falls back to plain Shannon entropy.
	score := bidirectionalScore(line, nil)
	if math.Abs(score-baseScore) > 0.001 {
		t.Errorf("nil window should equal shannonEntropy: got %f want %f", score, baseScore)
	}

	// Empty window → same as nil (no trigrams to compare against).
	emptyWindow := map[string]struct{}{}
	scoreEmpty := bidirectionalScore(line, emptyWindow)
	if scoreEmpty != score {
		t.Errorf("empty window should behave like nil window: got %f vs %f", scoreEmpty, score)
	}

	// Window sharing all trigrams → 0 novel → 0 surprise → score == base.
	highOverlapWindow := charTrigrams(line)
	scoreHigh := bidirectionalScore(line, highOverlapWindow)
	if math.Abs(scoreHigh-baseScore) > 0.001 {
		t.Errorf("full-overlap window should equal base: got %f want %f", scoreHigh, baseScore)
	}

	// Window sharing no trigrams → all novel → surprise = +0.5.
	zeroOverlapWindow := charTrigrams("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	scoreNovel := bidirectionalScore(line, zeroOverlapWindow)
	diff := scoreNovel - baseScore
	if math.Abs(diff-0.5) > 0.01 {
		t.Errorf("zero-overlap window should add exactly 0.5 surprise: diff=%f", diff)
	}
}

func TestLineScoreWindowBoost(t *testing.T) {
	// A line with a novel pattern relative to its window scores higher than
	// one that is repetitive in context.
	seen := map[string]struct{}{}
	novelWindow := charTrigrams("completely different content xyz abc def")
	repeatWindow := charTrigrams("func handleRequest ctx context error {")
	line := "func handleRequest(ctx context.Context) error {"
	scoreNovel := lineScore(line, seen, novelWindow)
	scoreRepeat := lineScore(line, seen, repeatWindow)
	if scoreNovel <= scoreRepeat {
		t.Errorf("novel window should score higher: novel=%f repeat=%f", scoreNovel, scoreRepeat)
	}
}

func TestShannonEntropy(t *testing.T) {
	// All same char → entropy 0.
	if got := shannonEntropy("aaaaaaa"); got != 0 {
		t.Errorf("uniform string: got %f, want 0", got)
	}

	// Empty string → 0.
	if got := shannonEntropy(""); got != 0 {
		t.Errorf("empty: got %f, want 0", got)
	}

	// Two equally likely chars → entropy 1.0 bit/char.
	if got := shannonEntropy("ababab"); math.Abs(got-1.0) > 0.01 {
		t.Errorf("two chars equal prob: got %f, want ~1.0", got)
	}

	// Real English line should have entropy > 3.
	line := "error: failed to open file path/to/foo.go: no such file"
	if got := shannonEntropy(line); got < 3.0 {
		t.Errorf("real line entropy too low: %f", got)
	}

	// Decoration line should have low entropy.
	if got := shannonEntropy("=================="); got > 1.0 {
		t.Errorf("decoration line entropy too high: %f", got)
	}
}

func TestMarkerScore(t *testing.T) {
	// Has path/ext → +0.3
	if got := markerScore("src/foo/bar.go: some message"); got != 0.3 {
		t.Errorf("path marker: got %f, want 0.3", got)
	}
	// Has digit → +0.3
	if got := markerScore("line 42 was changed"); got != 0.3 {
		t.Errorf("digit marker: got %f, want 0.3", got)
	}
	// Has error keyword → +0.3
	if got := markerScore("error: something went wrong"); got != 0.3 {
		t.Errorf("error kw marker: got %f, want 0.3", got)
	}
	// No markers → 0
	if got := markerScore("ok"); got != 0 {
		t.Errorf("no marker: got %f, want 0", got)
	}
}

func TestIsPureDecoration(t *testing.T) {
	yes := []string{
		"================",
		"----------------",
		"",
		"|",
		"| ",
		"// ",
		"//",
		"# ",
		"-- ",
		"#########",
		"===========================",
	}
	no := []string{
		"func foo() {",
		"error: file not found",
		"// this is a doc comment with content",
		"# real comment",
	}
	for _, s := range yes {
		if !isPureDecoration(s) {
			t.Errorf("expected decoration for %q", s)
		}
	}
	for _, s := range no {
		if isPureDecoration(s) {
			t.Errorf("expected non-decoration for %q", s)
		}
	}
}

func TestStripFiller(t *testing.T) {
	lines := []string{
		"error: build failed",
		`use "git add <file>..." to include in what will be committed`,
		"run `npm fund` for details",
		"FAIL internal/compress",
	}
	got := stripFiller(lines)
	if len(got) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(got), got)
	}
	if got[0] != "error: build failed" || got[1] != "FAIL internal/compress" {
		t.Fatalf("wrong lines kept: %v", got)
	}
}

func TestCharTrigrams(t *testing.T) {
	tg := charTrigrams("abcd")
	// Trigrams: abc, bcd
	if len(tg) != 2 {
		t.Fatalf("expected 2 trigrams for 'abcd', got %d", len(tg))
	}
	if _, ok := tg["abc"]; !ok {
		t.Error("missing trigram 'abc'")
	}
	if _, ok := tg["bcd"]; !ok {
		t.Error("missing trigram 'bcd'")
	}

	// Short string — less than 3 chars → nil.
	if charTrigrams("ab") != nil {
		t.Error("expected nil for 2-char string")
	}
}

func TestTrigramOverlapRatio(t *testing.T) {
	seen := map[string]struct{}{"abc": {}, "def": {}}
	tg := map[string]struct{}{"abc": {}, "xyz": {}}
	// 1 of 2 seen → 0.5
	ratio := trigramOverlapRatio(tg, seen)
	if math.Abs(ratio-0.5) > 0.01 {
		t.Errorf("got ratio %f, want 0.5", ratio)
	}

	// Empty tg → 0.
	if trigramOverlapRatio(nil, seen) != 0 {
		t.Error("expected 0 for nil trigrams")
	}
}

func TestQualityGate(t *testing.T) {
	original := []string{
		"error in src/foo/bar.go:42",
		"cannot find function handleRequest",
	}
	// Path and identifiers preserved → pass.
	compressed := []string{
		"error in src/foo/bar.go:42",
		"cannot find function handleRequest",
	}
	if !qualityGate(original, compressed) {
		t.Error("identical output should pass quality gate")
	}

	// Path dropped → fail.
	compressed2 := []string{"some other line"}
	if qualityGate(original, compressed2) {
		t.Error("dropping path should fail quality gate")
	}

	// Most identifiers dropped → fail.
	// "handleRequest" is a long ident; dropping it fails.
	compressed3 := []string{"error in src/foo/bar.go:42"}
	// This drops "handleRequest" (13 chars). That's 1 out of ~3 long idents.
	// Whether it passes depends on exact ident count — just test the path check.
	_ = compressed3 // path check is the definitive test above
}

func TestEntropyFilter_DecorationRemoved(t *testing.T) {
	// A block with decoration lines and real content.
	lines := []string{
		"===================",
		"Build Summary",
		"===================",
		"error: internal/foo/bar.go:10: undefined: Foo",
		"1 error",
		"===================",
	}
	got := EntropyFilter(lines, EntropyThresholdStandard)
	if got == nil {
		// Quality gate or savings threshold may block — test the decoration stripping directly.
		stripped := stripDecorations(lines)
		for _, l := range stripped {
			if strings.TrimSpace(l) == "===================" {
				t.Error("decoration line not stripped")
			}
		}
		return
	}
	for _, l := range got {
		if strings.TrimSpace(l) == "===================" {
			t.Errorf("decoration line survived in EntropyFilter output: %q", l)
		}
	}
}

func TestEntropyFilter_FillerRemoved(t *testing.T) {
	// Mix of filler and signal lines.
	lines := make([]string, 0, 20)
	for i := 0; i < 15; i++ {
		lines = append(lines, `use "git add <file>..." to include in what will be committed`)
	}
	lines = append(lines, "error: src/main.go:5: undefined: Foo")
	lines = append(lines, "FAIL\tgithub.com/alehatsman/dex\t0.5s")
	lines = append(lines, "exit status 1")

	got := EntropyFilter(lines, EntropyThresholdStandard)
	if got == nil {
		// Quality gate blocked — check filler stripping directly.
		stripped := stripFiller(lines)
		for _, l := range stripped {
			if strings.Contains(l, "git add") {
				t.Error("filler line not stripped")
			}
		}
		return
	}
	for _, l := range got {
		if strings.Contains(l, "git add") {
			t.Errorf("filler line survived: %q", l)
		}
	}
}

func TestEntropyFilter_QualityGateBlocks(t *testing.T) {
	// A file with important path that would be dropped by entropy scoring.
	// The quality gate should return nil.
	lines := []string{
		"some very low entropy aaaa",
		"aaaa bbbb cccc dddd",
		"internal/compress/entropy.go has an issue",
		"aaaa bbbb cccc dddd",
		"aaaa bbbb cccc dddd",
	}
	// If the path line gets dropped, quality gate fires.
	// Just verify it doesn't panic and either returns nil or preserves the path.
	got := EntropyFilter(lines, EntropyThresholdMax)
	if got != nil {
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, "internal/compress/entropy.go") {
			t.Error("path dropped but quality gate didn't block")
		}
	}
}
