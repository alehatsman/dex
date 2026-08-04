package retrieve

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// These cases moved down from internal/mcp when #113 relocated the inline
// orchestration into retrieve: the byte-budget inliner is InlineContentKeyed
// and the former mcp inlineContent wrapper is gone, so the tests call the
// retrieve func directly. inlineIsTest is the test-source classifier the
// transport injects (isTestPath) — it must be non-nil (the semantic-lane test
// filter calls it unguarded); isNonImpl is nil here since none of these cases
// exercise assemble coverage ordering.
func inlineIsTest(p string) bool { return strings.HasSuffix(p, "_test.go") }

func TestInlineSuggestedReadsBasic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "f.go"),
		"line 1\nline 2\nline 3\nline 4\nline 5\n")

	reads := []SuggestedRead{{Path: "f.go", StartLine: 2, EndLine: 4, Reason: "x"}}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)

	want := "line 2\nline 3\nline 4\n"
	if reads[0].Content != want {
		t.Errorf("content=%q want %q", reads[0].Content, want)
	}
	if reads[0].Truncated {
		t.Error("should not be truncated")
	}
}

func TestInlineSuggestedReadsPerReadLineCap(t *testing.T) {
	// Generate a 200-line file and ask for the whole thing. The
	// per-read cap (60 lines) should clip the content and flag it.
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeFile(t, filepath.Join(root, "big.go"), b.String())

	reads := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)

	if !reads[0].Truncated {
		t.Error("want truncated=true when range exceeds per-read cap")
	}
	got := strings.Count(reads[0].Content, "\n")
	if got > 60 {
		t.Errorf("got %d lines, want ≤60", got)
	}
	// EndLine on the wire stays as the original request so the
	// caller can issue a follow-up Read for the rest.
	if reads[0].EndLine != 200 {
		t.Errorf("EndLine=%d, want 200 (unchanged)", reads[0].EndLine)
	}
}

func TestInlineSuggestedReadsTotalByteBudget(t *testing.T) {
	// Six reads each at the per-read byte cap (4 KB) should exhaust the
	// 20 KB targeted-intent total budget before all are filled.
	root := t.TempDir()
	// Write a file with ~30 long lines so any 60-line slice hits the
	// per-read byte cap (4 KB) first.
	var b strings.Builder
	for range 30 {
		b.WriteString(strings.Repeat("x", 500))
		b.WriteByte('\n')
	}
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		writeFile(t, filepath.Join(root, n), b.String())
	}
	reads := []SuggestedRead{
		{Path: "a.go", StartLine: 1, EndLine: 30},
		{Path: "b.go", StartLine: 1, EndLine: 30},
		{Path: "c.go", StartLine: 1, EndLine: 30},
		{Path: "d.go", StartLine: 1, EndLine: 30},
		{Path: "e.go", StartLine: 1, EndLine: 30},
		{Path: "f.go", StartLine: 1, EndLine: 30},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)

	total := 0
	for _, r := range reads {
		total += len(r.Content)
	}
	if total > 20*1024 {
		t.Errorf("total inlined bytes %d > 20 KB cap", total)
	}
	// Last read should be empty — budget exhausted.
	if reads[len(reads)-1].Content != "" {
		t.Errorf("last read should be empty once budget is exhausted; got %d bytes", len(reads[len(reads)-1].Content))
	}
}

func TestInlineContentSemanticHitsAlsoFilled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "b.go"), "line A\nline B\nline C\n")

	reads := []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 3}}
	sem := []SemHit{
		{Path: "a.go", StartLine: 1, EndLine: 3},
		{Path: "b.go", StartLine: 1, EndLine: 3},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, sem, nil, inlineIsTest, nil)

	if sem[0].Content == "" {
		t.Error("semantic_hits[0] should be filled (cache-hit from suggested_reads)")
	}
	if sem[1].Content == "" {
		t.Error("semantic_hits[1] should be filled (separate file, within budget)")
	}
	if reads[0].Content == "" {
		t.Error("suggested_reads[0] should still be filled")
	}
}

func TestInlineContentSharedBudgetDoesNotDoubleCharge(t *testing.T) {
	// Same range appears in both lanes; the read cache should serve
	// the second request without re-charging the budget, so plenty
	// of headroom remains for other hits.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shared.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "other.go"), "x\ny\nz\n")

	reads := []SuggestedRead{{Path: "shared.go", StartLine: 1, EndLine: 3}}
	sem := []SemHit{
		{Path: "shared.go", StartLine: 1, EndLine: 3},
		{Path: "other.go", StartLine: 1, EndLine: 3},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, sem, nil, inlineIsTest, nil)

	if reads[0].Content == "" || sem[0].Content == "" || sem[1].Content == "" {
		t.Errorf("expected all three to be filled; got reads=%q sem0=%q sem1=%q",
			reads[0].Content, sem[0].Content, sem[1].Content)
	}
	if reads[0].Content != sem[0].Content {
		t.Errorf("dedup cache should return identical content")
	}
}

func TestInlineContentScoreFloorOnLowSignalQueries(t *testing.T) {
	// When top semantic score is below lowConfidenceScore, hits whose
	// individual score is below noiseFloorScore should ship without
	// Content (path+range only) — the agent keeps the pointer but we
	// don't burn bytes on what's almost certainly noise.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "weak.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "weaker.go"), "line A\nline B\nline C\n")
	writeFile(t, filepath.Join(root, "strong.go"), "line X\nline Y\nline Z\n")

	// Top score below lowConfidenceScore (0.45) triggers the floor.
	sem := []SemHit{
		{Path: "weak.go", StartLine: 1, EndLine: 3, Score: 0.42},   // top, < 0.45 → suppression mode on
		{Path: "weaker.go", StartLine: 1, EndLine: 3, Score: 0.38}, // < noiseFloorScore → suppressed
		// 0.41 is above the floor — would also be inlined.
	}
	InlineContentKeyed(root, IntentBehaviorSearch, nil, nil, sem, nil, inlineIsTest, nil)
	if sem[0].Content == "" {
		t.Error("top hit (score 0.42) should still inline — only sub-floor hits are suppressed")
	}
	if sem[1].Content != "" {
		t.Errorf("hit at score 0.38 should be suppressed under floor; got Content=%q", sem[1].Content)
	}

	// Sanity check: when top score IS strong, low-score companions
	// still inline normally (the floor only fires on no-signal pools).
	sem2 := []SemHit{
		{Path: "strong.go", StartLine: 1, EndLine: 3, Score: 0.80},
		{Path: "weaker.go", StartLine: 1, EndLine: 3, Score: 0.38},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, nil, nil, sem2, nil, inlineIsTest, nil)
	if sem2[1].Content == "" {
		t.Error("low-score companion to a strong top should still inline (floor only fires on no-signal queries)")
	}
}

func TestInlineSuggestedReadsMissingFileGraceful(t *testing.T) {
	root := t.TempDir()
	reads := []SuggestedRead{{Path: "does-not-exist.go", StartLine: 1, EndLine: 5}}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil) // must not panic
	if reads[0].Content != "" {
		t.Errorf("missing file should leave content empty, got %q", reads[0].Content)
	}
}

func TestInlineContentFillsSymbolBodyForSymbolLookup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"),
		"package main\n"+
			"\n"+
			"func Alpha() string {\n"+
			"\treturn \"alpha\"\n"+
			"}\n")
	syms := []SymbolHit{
		{QualifiedName: "Alpha", Path: "a.go", StartLine: 3, EndLine: 5},
	}
	InlineContentKeyed(root, IntentSymbolLookup, nil, syms, nil, nil, inlineIsTest, nil)
	if syms[0].Body == "" {
		t.Fatal("symbol body should be inlined for symbol_lookup intent")
	}
	if !strings.Contains(syms[0].Body, "return \"alpha\"") {
		t.Errorf("symbol body should include the function body; got %q", syms[0].Body)
	}
}

func TestInlineContentFillsImportsForGoFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "deep/foo.go"),
		"package deep\n"+
			"\n"+
			"import (\n"+
			"\t\"context\"\n"+
			"\t\"fmt\"\n"+
			"\t\"github.com/x/y\"\n"+
			")\n"+
			"\n"+
			"func A() {}\n"+
			"func B() {}\n"+
			"func C() {}\n"+
			"func D() {}\n"+
			"func E() {}\n"+
			"func F() {}\n"+
			"func G() {}\n"+
			"func H() {}\n"+
			"func I() {}\n"+
			"func J() {}\n"+
			"func K() {}\n"+
			"func L() {}\n"+
			"func M() {}\n"+
			"func N() {}\n")
	reads := []SuggestedRead{
		// Reads a function far from the import block (StartLine > 5).
		{Path: "deep/foo.go", StartLine: 20, EndLine: 22, Reason: "test"},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)
	if reads[0].Imports == "" {
		t.Fatal("Go imports should be inlined for a read starting away from the top")
	}
	for _, want := range []string{"import (", "\"context\"", "\"fmt\"", "\"github.com/x/y\"", ")"} {
		if !strings.Contains(reads[0].Imports, want) {
			t.Errorf("imports missing %q; got:\n%s", want, reads[0].Imports)
		}
	}
}

func TestInlineContentSkipsImportsWhenReadCoversTop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "foo.go"),
		"package foo\n\nimport \"context\"\n\nfunc A() {}\n")
	reads := []SuggestedRead{
		// StartLine=1 → the read already includes the import line.
		{Path: "foo.go", StartLine: 1, EndLine: 5, Reason: "test"},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)
	if reads[0].Imports != "" {
		t.Errorf("Imports should be omitted when the read already covers the top; got %q", reads[0].Imports)
	}
}

func TestInlineContentSkipsImportsForUnknownLanguage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "foo.txt"), "plain text\nno imports here\n")
	reads := []SuggestedRead{
		{Path: "foo.txt", StartLine: 20, EndLine: 22, Reason: "test"},
	}
	InlineContentKeyed(root, IntentBehaviorSearch, reads, nil, nil, nil, inlineIsTest, nil)
	if reads[0].Imports != "" {
		t.Errorf("unknown language should produce empty Imports; got %q", reads[0].Imports)
	}
}

func TestInlineContentSkipsSymbolBodyForOtherIntents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"),
		"package main\n\nfunc Alpha() {}\n")
	syms := []SymbolHit{
		{QualifiedName: "Alpha", Path: "a.go", StartLine: 3, EndLine: 3},
	}
	// behavior_search should NOT fill bodies — signature+doc are
	// considered sufficient and we save budget for semantic_hits.
	InlineContentKeyed(root, IntentBehaviorSearch, nil, syms, nil, nil, inlineIsTest, nil)
	if syms[0].Body != "" {
		t.Errorf("non-symbol_lookup intent should leave Body empty; got %q", syms[0].Body)
	}
}

// TestInlineContentSkipsTestSourceForNonEditing checks the test-path filter:
// the suppressLowScore branch (fake-embed cosines are too low to clear the
// confidence threshold) doesn't mask the filter's behavior. Scores are set
// above lowConfidenceScore so the test-path filter is the only suppression.
func TestInlineContentSkipsTestSourceForNonEditing(t *testing.T) {
	projDir := t.TempDir()
	implPath := filepath.Join(projDir, "greet.go")
	testPath := filepath.Join(projDir, "greet_test.go")
	writeFile(t, implPath,
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	writeFile(t, testPath,
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"x\") == \"\" {\n\t\tt.Fatal(\"oops\")\n\t}\n}\n")

	mkHits := func() []SemHit {
		return []SemHit{
			{Path: "greet.go", StartLine: 3, EndLine: 3, Score: 0.9, Kind: "function_declaration"},
			{Path: "greet_test.go", StartLine: 5, EndLine: 9, Score: 0.8, Kind: "function_declaration"},
		}
	}

	t.Run("behavior_search drops test content", func(t *testing.T) {
		sem := mkHits()
		InlineContentKeyed(projDir, IntentBehaviorSearch, nil, nil, sem, nil, inlineIsTest, nil)
		if sem[0].Content == "" {
			t.Errorf("impl greet.go should be inlined; got empty")
		}
		if sem[1].Content != "" {
			t.Errorf("test greet_test.go should be path-only; got %d bytes", len(sem[1].Content))
		}
	})

	t.Run("architecture drops test content", func(t *testing.T) {
		sem := mkHits()
		InlineContentKeyed(projDir, IntentArchitecture, nil, nil, sem, nil, inlineIsTest, nil)
		if sem[1].Content != "" {
			t.Errorf("architecture: test should be path-only; got %d bytes", len(sem[1].Content))
		}
	})

	t.Run("editing_context keeps test content", func(t *testing.T) {
		sem := mkHits()
		InlineContentKeyed(projDir, IntentEditingContext, nil, nil, sem, nil, inlineIsTest, nil)
		if sem[1].Content == "" {
			t.Errorf("editing_context: sibling test should be inlined; got empty")
		}
	})
}

func TestInlineSuggestedReadsExplorationDenser(t *testing.T) {
	// 200-line file requested in full. targeted caps clip at 60 lines;
	// exploration caps should clip at 120.
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeFile(t, filepath.Join(root, "big.go"), b.String())

	targeted := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	InlineContentKeyed(root, IntentBehaviorSearch, targeted, nil, nil, nil, inlineIsTest, nil)
	targetedLines := strings.Count(targeted[0].Content, "\n")

	exploration := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	InlineContentKeyed(root, IntentArchitecture, exploration, nil, nil, nil, inlineIsTest, nil)
	explorationLines := strings.Count(exploration[0].Content, "\n")

	if !(explorationLines > targetedLines) {
		t.Errorf("exploration should include more lines than targeted; got %d vs %d", explorationLines, targetedLines)
	}
	if explorationLines > 120 {
		t.Errorf("exploration line count %d exceeds expected 120 cap", explorationLines)
	}
}
