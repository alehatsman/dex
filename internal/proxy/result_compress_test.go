package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestShouldPreserveResult_DexCompressed(t *testing.T) {
	// dex shell output with saved_pct > 0 (omitempty means it's only present when > 0)
	text := `{"output":"ls -la\n...", "exit_code":0, "saved_pct":45}`
	if !shouldPreserveResult(text) {
		t.Error("dex result with saved_pct should trigger shouldPreserveResult")
	}
}

func TestShouldPreserveResult_DexZeroSavedPct(t *testing.T) {
	// saved_pct=0 is omitted by omitempty, so it won't appear in text
	text := `{"output":"short output", "exit_code":0}`
	if shouldPreserveResult(text) {
		t.Error("dex result without saved_pct should not be preserved by this check")
	}
}

func TestShouldPreserveResult_LCSafe(t *testing.T) {
	text := "some output <lc_safe>important verbatim content</lc_safe> more output"
	if !shouldPreserveResult(text) {
		t.Error("content with <lc_safe> should be preserved")
	}
}

func TestShouldPreserveResult_PlainText(t *testing.T) {
	text := "regular shell output with no markers"
	if shouldPreserveResult(text) {
		t.Error("plain text should not be preserved")
	}
}

func TestIsDexResult(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`{"output":"ls","exit_code":0,"saved_pct":45}`, true},
		{`{"answer":"some answer","saved_pct":12}`, true},
		{`{"output":"short","exit_code":0}`, false}, // no saved_pct
		{"plain text without markers", false},
		// Note: saved_pct=0 is omitted by omitempty in the dex server so the
		// string literal `"saved_pct":0` never appears in real traffic.
	}
	for _, c := range cases {
		got := isDexResult(c.text)
		if got != c.want {
			t.Errorf("isDexResult(%q) = %v, want %v", c.text[:min(len(c.text), 50)], got, c.want)
		}
	}
}

func TestCompactDexStub_Ask(t *testing.T) {
	text := `{"answer":"Token counting is in countBodyTokens at baseline.go:29","status":"ok","saved_pct":30}`
	stub, ok := compactDexStub(text, "ask")
	if !ok {
		t.Fatal("expected ok=true for ask result")
	}
	if !strings.Contains(stub, "ask") {
		t.Errorf("stub should contain tool name: %q", stub)
	}
	if !strings.Contains(stub, "countBodyTokens") {
		t.Errorf("stub should contain key finding: %q", stub)
	}
	if !strings.Contains(stub, "re-query") {
		t.Errorf("stub should suggest re-query: %q", stub)
	}
}

func TestCompactDexStub_Shell(t *testing.T) {
	text := `{"output":"ok\nsome more output\n","exit_code":0,"saved_pct":55}`
	stub, ok := compactDexStub(text, "shell")
	if !ok {
		t.Fatal("expected ok=true for shell result")
	}
	if !strings.Contains(stub, "shell") {
		t.Errorf("stub should contain tool name: %q", stub)
	}
	if !strings.Contains(stub, "exit=0") {
		t.Errorf("stub should contain exit code: %q", stub)
	}
	// Only first line of output should be present.
	if strings.Contains(stub, "some more output") {
		t.Errorf("stub should not contain subsequent lines: %q", stub)
	}
}

func TestCompactDexStub_Find(t *testing.T) {
	text := `{"hits":[{"path":"a.go"},{"path":"b.go"},{"path":"c.go"}],"saved_pct":20}`
	stub, ok := compactDexStub(text, "find")
	if !ok {
		t.Fatal("expected ok=true for find result")
	}
	if !strings.Contains(stub, "3 hits") {
		t.Errorf("stub should contain hit count: %q", stub)
	}
}

func TestCompactDexStub_Grep(t *testing.T) {
	text := `{"matches":[{"line":1},{"line":2}],"saved_pct":10}`
	stub, ok := compactDexStub(text, "grep")
	if !ok {
		t.Fatal("expected ok=true for grep result")
	}
	if !strings.Contains(stub, "2 matches") {
		t.Errorf("stub should contain match count: %q", stub)
	}
}

func TestCompactDexStub_Fallback(t *testing.T) {
	// JSON dex result but no recognisable field.
	text := `{"status":"ok","saved_pct":5}`
	stub, ok := compactDexStub(text, "unknown")
	if !ok {
		t.Fatal("expected ok=true even for unrecognised dex result")
	}
	if !strings.Contains(stub, "pruned") {
		t.Errorf("stub should mention pruned: %q", stub)
	}
}

func TestCompactDexStub_NonJSON(t *testing.T) {
	_, ok := compactDexStub("not json", "ask")
	if ok {
		t.Error("expected ok=false for non-JSON input")
	}
}

func TestCompactDexStub_AnswerTruncated(t *testing.T) {
	// Answer longer than maxRunes (200) should be truncated with ellipsis.
	longAnswer := strings.Repeat("x", 300)
	text := fmt.Sprintf(`{"answer":%q,"saved_pct":1}`, longAnswer)
	stub, ok := compactDexStub(text, "ask")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(stub, "…") {
		t.Errorf("stub should contain ellipsis for truncated answer: %q", stub)
	}
	// Stub should be much shorter than the original.
	if len(stub) >= len(longAnswer) {
		t.Errorf("stub (%d chars) should be shorter than original answer (%d chars)", len(stub), len(longAnswer))
	}
}

func TestIsTestOutput_GoTestRunner(t *testing.T) {
	cases := []struct {
		text   string
		isTest bool
	}{
		{"=== RUN TestFoo\n--- PASS TestFoo (0.01s)", true},
		{"--- FAIL TestBar (0.02s)\nFAIL\tgithub.com/x/y", true},
		{"FAIL\tgithub.com/x/y 1.234s", true},
		{"regular command output", false},
		{"error[E0308]: expected type, found `42`", true},    // Rust
		{"warning[dead_code]: function is never used", true}, // Rust
		{"test result: ok. 5 passed; 0 failed", true},        // Rust test runner
		{"BUILD FAILED in 2s", true},                         // Gradle
		{"BUILD SUCCESSFUL in 3s", true},                     // Gradle
	}
	for _, c := range cases {
		got := isTestOutput(c.text)
		if got != c.isTest {
			t.Errorf("isTestOutput(%q) = %v, want %v", c.text[:min(len(c.text), 40)], got, c.isTest)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRewriteBlock_CompactsDexResult verifies that large dex tool results with
// saved_pct are compacted to short stubs rather than kept verbatim.
func TestRewriteBlock_CompactsDexResult(t *testing.T) {
	toolNames := map[string]string{"r1": "shell"}
	// Realistic dex shell result: long output field that makes compaction worthwhile.
	longOutput := strings.Repeat("build output line\n", 30) // ~540 chars
	dexResult := fmt.Sprintf(`{"output":%q,"exit_code":0,"saved_pct":55}`, longOutput)
	blk := makeToolResultBlock("r1", dexResult)

	rewritten, changed, preserved := rewriteBlock(blk, toolNames, nil)
	if !changed {
		t.Error("dex compressed result should be compacted (changed=true)")
	}
	if preserved {
		t.Error("dex compressed result should not be marked as preserved after compaction")
	}
	// Compacted block should be much shorter than the original.
	if len(rewritten) >= len(blk) {
		t.Errorf("compacted block (%d bytes) should be smaller than original (%d bytes)", len(rewritten), len(blk))
	}
	// Should contain a hint to re-run.
	if !strings.Contains(string(rewritten), "pruned") {
		t.Errorf("compacted block should mention pruned: %s", rewritten)
	}
}

// TestRewriteBlock_TinyDexResultPreserved verifies that very small dex results
// (where the stub would be longer than the original) are preserved verbatim.
func TestRewriteBlock_TinyDexResultPreserved(t *testing.T) {
	toolNames := map[string]string{"r1": "shell"}
	// Very short dex result: stub would be longer, so preserve.
	dexResult := `{"output":"ok","exit_code":0,"saved_pct":5}`
	blk := makeToolResultBlock("r1", dexResult)

	_, changed, preserved := rewriteBlock(blk, toolNames, nil)
	if changed {
		t.Error("tiny dex result should be preserved verbatim (stub would be larger)")
	}
	if !preserved {
		t.Error("tiny dex result should be marked as preserved")
	}
}

// TestRewriteBlock_LCSafePreservedVerbatim verifies that <lc_safe> content
// is still kept byte-identical even though it triggers shouldPreserveResult.
func TestRewriteBlock_LCSafePreservedVerbatim(t *testing.T) {
	toolNames := map[string]string{"r1": "Bash"}
	// <lc_safe> content long enough to exceed minPruneChars.
	content := "<lc_safe>critical verbatim section</lc_safe>\n"
	for len(content) < minPruneChars*2 {
		content += "padding line\n"
	}
	blk := makeToolResultBlock("r1", content)

	rewritten, changed, preserved := rewriteBlock(blk, toolNames, nil)
	if changed {
		t.Error("<lc_safe> content should not be changed")
	}
	if !preserved {
		t.Error("<lc_safe> content should be marked as preserved")
	}
	if string(rewritten) != string(blk) {
		t.Error("<lc_safe> content must be byte-identical")
	}
}

// TestRewriteBlock_PreservesTestOutput verifies that test output is not stub-replaced.
func TestRewriteBlock_PreservesTestOutput(t *testing.T) {
	toolNames := map[string]string{"s1": "Bash"}
	// Build enough content to exceed minPruneChars.
	testOut := "=== RUN TestFoo\n--- PASS TestFoo (0.01s)\n"
	for len(testOut) < minPruneChars*2 {
		testOut += "--- PASS TestExtra (0.00s)\n"
	}
	blk := makeToolResultBlock("s1", testOut)

	_, changed, preserved := rewriteBlock(blk, toolNames, nil)
	if changed {
		t.Error("test output should not be changed")
	}
	if !preserved {
		t.Error("test output should be marked as preserved")
	}
}

// TestRewriteBlock_StillStubsNonPreserved verifies existing stub behavior is intact.
func TestRewriteBlock_StillStubsNonPreserved(t *testing.T) {
	toolNames := map[string]string{"r1": "mcp__dex__read"}
	// Long file content without any preserved markers.
	content := "package foo\n\n// lots of code...\n"
	for len(content) < minPruneChars*2 {
		content += "func Foo() { /* code */ }\n"
	}
	blk := makeToolResultBlock("r1", content)

	_, changed, preserved := rewriteBlock(blk, toolNames, nil)
	if !changed {
		t.Error("non-preserved file read should be stubbed (changed)")
	}
	if preserved {
		t.Error("non-preserved file read should not be marked as preserved")
	}
}

// TestPruneHistoryWithStats_DexResultsCompacted verifies that dex results in
// the old region are compacted (not preserved verbatim) and that
// ResultsPreserved stays zero for pure dex traffic.
func TestPruneHistoryWithStats_DexResultsCompacted(t *testing.T) {
	// Use a realistic long output so the compact stub is actually shorter.
	longOut := strings.Repeat("build output line\n", 20) // ~360 chars
	dexResult := fmt.Sprintf(`{"output":%q,"exit_code":0,"saved_pct":40}`, longOut)

	// Build a conversation with 18 old-region pairs so pruneStart returns > 0.
	var msgs []json.RawMessage
	for i := 0; i < 18; i++ {
		id := fmt.Sprintf("s%d", i)
		msgs = append(msgs,
			mustMarshal(map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": id, "name": "shell",
						"input": map[string]any{"command": "ls"}},
				},
			}),
			mustMarshal(map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": id, "content": dexResult},
				},
			}),
		)
	}
	// keepRecent=4 leaves the last 4 messages in the keep window.
	// With 36 total messages, pruneStart(36,4) = (32/16)*16 = 32.
	// Messages 0–31 are in the old region; all have dex tool results.
	out, st := PruneHistoryWithStats(msgs, 4)

	// Dex results are now compacted, not preserved.
	if st.ResultsPreserved != 0 {
		t.Errorf("expected ResultsPreserved=0 for dex-only traffic, got %d", st.ResultsPreserved)
	}
	if st.TokensPreserved != 0 {
		t.Errorf("expected TokensPreserved=0 for dex-only traffic, got %d", st.TokensPreserved)
	}

	// Old-region messages should have been rewritten (compacted).
	if len(out) != len(msgs) {
		t.Fatalf("message count changed: got %d, want %d", len(out), len(msgs))
	}
	// Spot-check: the first user message (index 1, in old region) should no
	// longer contain the full dex JSON.
	if strings.Contains(string(out[1]), `"saved_pct"`) {
		t.Error("old-region dex result should have been compacted, not kept verbatim")
	}
	if !strings.Contains(string(out[1]), "pruned") {
		t.Error("compacted stub should mention 'pruned'")
	}
	// Keep-window messages (indices 32+) should be byte-identical.
	for i := 32; i < len(msgs); i++ {
		if string(out[i]) != string(msgs[i]) {
			t.Errorf("keep-window message %d should be unchanged", i)
		}
	}
}

// makeToolResultBlock builds a minimal tool_result block JSON for testing.
func makeToolResultBlock(toolUseID, content string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
		"content":     content,
	})
	return b
}

// mustMarshal marshals v to JSON or panics (test helper).
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustMarshal: %v", err))
	}
	return b
}
