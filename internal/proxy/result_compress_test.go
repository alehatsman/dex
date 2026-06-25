package proxy

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestShouldPreserveResult_DexCompressed(t *testing.T) {
	// dex shell output with saved_pct > 0 (omitempty means it's only present when > 0)
	text := `{"output":"ls -la\n...", "exit_code":0, "saved_pct":45}`
	if !shouldPreserveResult(text) {
		t.Error("dex result with saved_pct should be preserved")
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

// TestRewriteBlock_PreservesDexCompressed verifies that tool_result blocks
// from dex tools with saved_pct > 0 are not replaced with stubs.
func TestRewriteBlock_PreservesDexCompressed(t *testing.T) {
	toolNames := map[string]string{"r1": "mcp__dex__shell"}
	dexResult := `{"output":"compressed output","exit_code":0,"saved_pct":55}`
	blk := makeToolResultBlock("r1", dexResult)

	rewritten, changed, preserved := rewriteBlock(blk, toolNames)
	if changed {
		t.Error("dex compressed result should not be changed")
	}
	if !preserved {
		t.Error("dex compressed result should be marked as preserved")
	}
	// Content must be byte-identical.
	if string(rewritten) != string(blk) {
		t.Errorf("rewritten content differs from original:\noriginal: %s\nrewritten: %s", blk, rewritten)
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

	_, changed, preserved := rewriteBlock(blk, toolNames)
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

	_, changed, preserved := rewriteBlock(blk, toolNames)
	if !changed {
		t.Error("non-preserved file read should be stubbed (changed)")
	}
	if preserved {
		t.Error("non-preserved file read should not be marked as preserved")
	}
}

// TestPruneHistoryWithStats_CountsPreserved checks that PruneHistoryWithStats
// correctly counts preserved results when the old region is large enough to
// exceed PruneStride (16).
func TestPruneHistoryWithStats_CountsPreserved(t *testing.T) {
	dexResult := `{"output":"some output","exit_code":0,"saved_pct":40}`

	// Build a conversation with 18 old-region pairs so pruneStart returns > 0.
	var msgs []json.RawMessage
	for i := 0; i < 18; i++ {
		id := fmt.Sprintf("s%d", i)
		msgs = append(msgs,
			mustMarshal(map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": id, "name": "mcp__dex__shell",
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
	// Messages 0-31 are in the old region.
	_, st := PruneHistoryWithStats(msgs, 4)
	if st.ResultsPreserved == 0 {
		t.Error("expected at least one preserved result in the old region")
	}
	if st.TokensPreserved <= 0 {
		t.Errorf("expected non-zero tokens preserved, got %d", st.TokensPreserved)
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
