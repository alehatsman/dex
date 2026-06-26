package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// makeMessages encodes a slice of interface{} values as json.RawMessage.
func makeMessages(t *testing.T, msgs ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		out[i] = b
	}
	return out
}

// toolUseMsg builds an assistant message with one tool_use block.
func toolUseMsg(id, name string) any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": map[string]any{},
			},
		},
	}
}

// toolResultMsg builds a user message with one tool_result block.
func toolResultMsg(toolUseID, content string) any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     content,
			},
		},
	}
}

// extractFirstToolResultContent returns the content string of the first
// tool_result block in the first user message of messages.
func extractFirstToolResultContent(t *testing.T, messages []json.RawMessage) string {
	t.Helper()
	for _, raw := range messages {
		var msg struct {
			Role    string `json:"role"`
			Content []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
			continue
		}
		for _, blk := range msg.Content {
			if blk.Type == "tool_result" {
				return blk.Content
			}
		}
	}
	t.Fatal("no tool_result content block found")
	return ""
}

// longContent builds a string longer than minPruneChars.
func longContent(lines int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("line content here\n")
	}
	return b.String()
}

// TestOldFileReadGetsHonestRereadStub is the port of the lean-ctx test of the
// same name. A Read tool_result older than keep_recent must be replaced with
// the honest re-read stub — never a partial excerpt.
func TestOldFileReadGetsHonestRereadStub(t *testing.T) {
	fileContent := longContent(20) // 20 lines, well above minPruneChars

	// With PruneStride=16 and keepRecent=10, we need at least 26 messages so
	// the prune zone (len-keepRecent) >= 16 and rounds to 16 rather than 0.
	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", fileContent),
	)
	// Pad to 26 total: 24 padding + the 2 above.
	for i := 1; i <= 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "later message"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)

	// Must contain the re-read stub phrase.
	if !strings.Contains(got, "pruned to save tokens; re-read if needed") {
		t.Errorf("expected re-read stub, got: %q", got)
	}
	// Must NOT contain any fragment of the original file content — no partial leakage.
	if strings.Contains(got, "line content here") {
		t.Errorf("stub must not contain original file body; got: %q", got)
	}
	// Must mention line count.
	if !strings.Contains(got, "20 lines") {
		t.Errorf("stub should mention line count; got: %q", got)
	}
}

// TestRecentMessagesUntouched asserts that messages within keepRecent are not
// rewritten, even if they contain bulky tool_result content.
func TestRecentMessagesUntouched(t *testing.T) {
	fileContent := longContent(30)

	// Only 2 messages, keepRecent=10 → both are in the window.
	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", fileContent),
	)
	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)
	if !strings.Contains(got, "line content here") {
		t.Errorf("recent tool_result should be untouched; got: %q", got)
	}
}

// TestCommandOutputGetsHeadTailSummary checks that command/bash tool_results
// get a head+tail summary rather than a re-read stub.
func TestCommandOutputGetsHeadTailSummary(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "output line "+string(rune('A'+i)))
	}
	cmdOutput := strings.Join(lines, "\n") + "\n"

	// With PruneStride=16 and keepRecent=10, need >=26 messages so the prune zone
	// rounds up to 16 rather than 0.
	msgs := makeMessages(t,
		toolUseMsg("id1", "Bash"),
		toolResultMsg("id1", cmdOutput),
	)
	// Pad to 26 total.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)

	if !strings.Contains(got, "lines pruned") {
		t.Errorf("expected head/tail summary with pruned count; got: %q", got)
	}
	// Must not contain re-read stub.
	if strings.Contains(got, "re-read if needed") {
		t.Errorf("Bash output should not get re-read stub; got: %q", got)
	}
	// First line of original output must be present.
	if !strings.Contains(got, "output line A") {
		t.Errorf("head lines should be present; got: %q", got)
	}
}

// TestShortContentSkipped ensures content below minPruneChars is not rewritten.
func TestShortContentSkipped(t *testing.T) {
	shortContent := "short output"

	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", shortContent),
	)
	// Need >=26 messages with keepRecent=10 so stride boundary fires at 16.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)
	if got != shortContent {
		t.Errorf("short content should be untouched; got: %q", got)
	}
}

// TestUnknownToolGetsHeadTailFallback ensures unknown tool names get the
// conservative head/tail summary (not a re-read stub).
func TestUnknownToolGetsHeadTailFallback(t *testing.T) {
	content := longContent(20)

	msgs := makeMessages(t,
		toolUseMsg("id1", "some_exotic_tool_xyz"),
		toolResultMsg("id1", content),
	)
	// Need >=26 messages with keepRecent=10 so stride boundary fires at 16.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)
	if !strings.Contains(got, "lines pruned") {
		t.Errorf("unknown tool should get head/tail summary; got: %q", got)
	}
	if strings.Contains(got, "re-read if needed") {
		t.Errorf("unknown tool should not get re-read stub; got: %q", got)
	}
}

// TestMalformedMessageFailOpen ensures a malformed message passes through
// unmodified rather than being dropped or panicking.
func TestMalformedMessageFailOpen(t *testing.T) {
	bad := json.RawMessage(`{not valid json`)
	msgs := []json.RawMessage{bad}
	pruned := PruneHistory(msgs, 0)
	if string(pruned[0]) != string(bad) {
		t.Errorf("malformed message should pass through untouched")
	}
}

// TestAssistantMessagesUntouched ensures assistant messages (which have tool_use
// blocks, not tool_result) are never rewritten.
func TestAssistantMessagesUntouched(t *testing.T) {
	asst := toolUseMsg("id1", "Read")
	msgs := makeMessages(t, asst)
	pruned := PruneHistory(msgs, 0)
	var orig, got map[string]any
	_ = json.Unmarshal(msgs[0], &orig)
	_ = json.Unmarshal(pruned[0], &got)
	origJSON, _ := json.Marshal(orig)
	gotJSON, _ := json.Marshal(got)
	if string(origJSON) != string(gotJSON) {
		t.Errorf("assistant message should be untouched\norig: %s\n got: %s", origJSON, gotJSON)
	}
}

// TestArrayContentToolResult checks that tool_result with array content (not
// bare string) is also pruned correctly.
func TestArrayContentToolResult(t *testing.T) {
	arrayContent := make([]map[string]any, 1)
	arrayContent[0] = map[string]any{"type": "text", "text": longContent(25)}

	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "id1",
					"content":     arrayContent,
				},
			},
		},
	)
	// Need >=26 messages with keepRecent=10 so stride boundary fires at 16.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	got := extractFirstToolResultContent(t, pruned)
	if !strings.Contains(got, "re-read if needed") {
		t.Errorf("array-content tool_result should get re-read stub; got: %q", got)
	}
}

// TestHeadTailSummary unit-tests the helper directly.
func TestHeadTailSummary(t *testing.T) {
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = strings.Repeat("x", 20)
	}
	s := strings.Join(lines, "\n") + "\n"
	summary := headTailSummary(s)
	if !strings.Contains(summary, "5 lines pruned") {
		t.Errorf("expected 5 lines pruned annotation; got: %q", summary)
	}
}

// TestPruneStartStride verifies pruneStart properties:
//   - Returns 0 when nothing is pruneable (msgLen <= keepRecent).
//   - Returns the exact raw boundary when raw < PruneStride (first-pass pruning
//     starts immediately, not delayed until a full stride accumulates).
//   - Returns a multiple of PruneStride once raw >= PruneStride (cache-warm).
//   - Is monotonically non-decreasing as msgLen grows.
func TestPruneStartStride(t *testing.T) {
	keepRecent := 8
	prev := 0
	for msgLen := 0; msgLen <= 64; msgLen++ {
		raw := msgLen - keepRecent
		got := pruneStart(msgLen, keepRecent)
		switch {
		case raw <= 0:
			// Nothing to prune: must return 0.
			if got != 0 {
				t.Errorf("pruneStart(%d, %d) = %d, want 0 (nothing to prune)",
					msgLen, keepRecent, got)
			}
		case raw < PruneStride:
			// First-pass: exact boundary, enables pruning without waiting for stride.
			if got != raw {
				t.Errorf("pruneStart(%d, %d) = %d, want raw=%d (first-pass exact boundary)",
					msgLen, keepRecent, got, raw)
			}
		default:
			// Stride-rounded: must be a positive multiple of PruneStride.
			if got == 0 || got%PruneStride != 0 {
				t.Errorf("pruneStart(%d, %d) = %d, not a multiple of PruneStride (%d)",
					msgLen, keepRecent, got, PruneStride)
			}
		}
		// Must be non-decreasing in all cases.
		if got < prev {
			t.Errorf("pruneStart(%d, %d) = %d < prev %d (non-monotonic)",
				msgLen, keepRecent, got, prev)
		}
		prev = got
	}
}

// msgWithCacheControl builds a user message that carries a top-level
// cache_control marker (simulating what the client sets as a breakpoint).
func msgWithCacheControl(text string) any {
	return map[string]any{
		"role":          "user",
		"content":       text,
		"cache_control": map[string]any{"type": "ephemeral"},
	}
}

// msgWithCacheControlInContent builds a user message whose content array has a
// block carrying cache_control (level-2 nesting).
func msgWithCacheControlInContent(text string) any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":          "text",
				"text":          text,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
	}
}

// TestCacheControlPrefixPreserved checks that messages up through the last
// cache_control marker are left byte-identical after PruneHistory, even when
// they fall inside the prunable zone.
func TestCacheControlPrefixPreserved(t *testing.T) {
	fileContent := longContent(20)

	// Messages 0-3: tool pair + padding + cache_control at index 3.
	// Total 20 messages, keepRecent=4: pruneStart(20,4)=16, pruneFloor=4.
	// Prune zone is [4,16); messages 0-3 are protected by pruneFloor.
	// Without pruneFloor, message 1 (index 1 < 16) would be pruned.
	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", fileContent), // index 1 — in stride zone, must survive via pruneFloor
		map[string]any{"role": "user", "content": "padding2"},
		msgWithCacheControl("breakpoint here"), // index 3 — last cache_control marker → pruneFloor=4
	)
	// Pad to 20 total.
	for i := 4; i < 20; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}
	pruned := PruneHistory(msgs, 4)

	// Messages 0-3 must be byte-identical to inputs.
	for i := 0; i <= 3; i++ {
		if string(pruned[i]) != string(msgs[i]) {
			t.Errorf("message[%d] changed despite being inside cache_control prefix\norig: %s\n got: %s",
				i, msgs[i], pruned[i])
		}
	}
	// The tool_result at index 1 must still contain original content (not a stub).
	got := extractFirstToolResultContent(t, pruned[:2])
	if !strings.Contains(got, "line content here") {
		t.Errorf("tool_result inside cache prefix should be untouched; got: %q", got)
	}
}

// TestCacheControlNoMarkers verifies that histories with no cache_control
// markers behave exactly as before (boundary driven purely by pruneStart).
func TestCacheControlNoMarkers(t *testing.T) {
	fileContent := longContent(20)

	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", fileContent),
	)
	// Need >=26 messages with keepRecent=10 so the prune zone (len-keepRecent)
	// rounds up to 16 via PruneStride, placing msg 0-1 in the prunable zone.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, DefaultKeepRecent)
	// With no markers, pruneFloor=0, boundary=pruneStart(26,10)=16.
	// Messages 0-15 (including the tool_result at index 1) are eligible for pruning.
	got := extractFirstToolResultContent(t, pruned)
	if !strings.Contains(got, "pruned to save tokens; re-read if needed") {
		t.Errorf("expected prune stub with no cache markers; got: %q", got)
	}
}

// TestCacheControlBoundaryFloor checks that the prune floor correctly protects
// the cache_control prefix while still pruning eligible messages outside it.
// Setup: 20 messages total, keepRecent=4 → pruneStart(20,4)=(16/16)*16=16.
// cache_control at index 3 → pruneFloor=4. Prune zone = [4, 16).
// Messages 0-3 must be untouched; message 5 (tool_result outside floor) pruned.
func TestCacheControlBoundaryFloor(t *testing.T) {
	fileContent := longContent(20)

	// Indices 0-3: tool pair + padding + cache_control marker.
	msgs := makeMessages(t,
		toolUseMsg("id1", "Read"),
		toolResultMsg("id1", fileContent),                // index 1 — inside floor, must NOT be pruned
		map[string]any{"role": "user", "content": "p"},   // index 2
		msgWithCacheControlInContent("cc block level 2"), // index 3 — last cache marker → pruneFloor=4
		toolUseMsg("id2", "Read"),
		toolResultMsg("id2", fileContent), // index 5 — in [4,16) → MUST be pruned
	)
	// Pad to 20 total (indices 6-19).
	for i := 6; i < 20; i++ {
		msgs = append(msgs, makeMessages(t,
			map[string]any{"role": "user", "content": "padding"},
		)...)
	}

	pruned := PruneHistory(msgs, 4)

	// Messages 0-3 must be byte-identical (inside pruneFloor).
	for i := 0; i <= 3; i++ {
		if string(pruned[i]) != string(msgs[i]) {
			t.Errorf("message[%d] changed despite being inside cache_control prefix", i)
		}
	}

	// Message 5 (tool_result for id2) is in the prune zone [4, 16) and should be pruned.
	var secondResultContent string
	for _, raw := range pruned[4:6] {
		var msg struct {
			Role    string `json:"role"`
			Content []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
			continue
		}
		for _, blk := range msg.Content {
			if blk.Type == "tool_result" {
				secondResultContent = blk.Content
			}
		}
	}
	if !strings.Contains(secondResultContent, "pruned to save tokens; re-read if needed") {
		t.Errorf("tool_result outside cache prefix should be pruned; got: %q", secondResultContent)
	}
}

// TestClassifyTool unit-tests tool name classification.
func TestClassifyTool(t *testing.T) {
	cases := []struct {
		name string
		want toolKind
	}{
		{"Read", kindFileRead},
		{"file_view", kindFileRead},
		{"str_replace_based_edit_tool_Read", kindFileRead},
		{"Bash", kindCommand},
		{"bash", kindCommand},
		{"Shell", kindCommand},
		{"search_files", kindCommand},
		{"some_exotic_tool", kindUnknown},
		{"", kindUnknown},
	}
	for _, c := range cases {
		got := classifyTool(c.name)
		if got != c.want {
			t.Errorf("classifyTool(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClassifyByContent covers the #615 content fallback: a foreign tool whose
// result looks like source code is classified kindFileRead; everything else
// stays kindUnknown (which prunes identically to a command).
func TestClassifyByContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want toolKind
	}{
		{"go source", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n", kindFileRead},
		{"python source", "import os\n\ndef handler(req):\n    return os.getcwd()\n", kindFileRead},
		{"typescript source", "export function add(a: number, b: number): number {\n  return a + b;\n}\n", kindFileRead},
		{"c source", "#include <stdio.h>\nint main(void) { return 0; }\n", kindFileRead},
		{"prose with 'return'", "Please return the package to the front desk by Friday afternoon.", kindUnknown},
		{"json config", "{\n  \"name\": \"dex\",\n  \"version\": \"1.0.0\"\n}\n", kindUnknown},
		{"log output", "2026-06-26 10:00:01 INFO  starting server\n2026-06-26 10:00:02 WARN  slow query\n", kindUnknown},
		// Regression: keywords mid-line must NOT match (line-start only, #615).
		{"git log oneline", "a1b2c3d feat(graph): package filter accepts a path suffix (#583)\nd4e5f6a fix(read): handle empty file\n", kindUnknown},
		{"markdown table", "| `func` | a function | no |\n| `import` | bring in a module | yes |\n", kindUnknown},
		{"yaml with shell", "tasks:\n  build:\n    steps:\n      - shell: go build ./... (package main)\n", kindUnknown},
		{"empty", "", kindUnknown},
	}
	for _, c := range cases {
		if got := classifyByContent(c.body); got != c.want {
			t.Errorf("classifyByContent(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPruneForeignFileReadByContent is the end-to-end guard: a foreign MCP tool
// (unknown name) returning source code must be pruned with the FILE-READ stub
// (line count preserved), not the command head/tail summary (#615).
func TestPruneForeignFileReadByContent(t *testing.T) {
	var body strings.Builder
	body.WriteString("package widget\n\nimport \"fmt\"\n\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&body, "func F%d() { fmt.Println(%d) }\n", i, i)
	}
	contentJSON, _ := json.Marshal(body.String())
	blk := map[string]json.RawMessage{
		"type":        json.RawMessage(`"tool_result"`),
		"tool_use_id": json.RawMessage(`"t1"`),
		"content":     contentJSON,
	}
	raw, _ := json.Marshal(blk)

	// Foreign tool name with no read/command keyword → name-classify whiffs,
	// content fallback must recognize the source body.
	out, pruned, _ := rewriteBlock(raw, map[string]string{"t1": "load_document"}, nil)
	if !pruned {
		t.Fatal("expected the large source result to be pruned")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var stub string
	if err := json.Unmarshal(got["content"], &stub); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub, "file read") || !strings.Contains(stub, "lines") {
		t.Errorf("expected the file-read stub (lines preserved), got %q", stub)
	}
}

// TestPruneHistoryPreservesPairingInvariant locks the proxy's single
// load-bearing correctness property (#652): pruning REWRITES messages in place
// and never drops one, so every assistant tool_use keeps its matching user
// tool_result. The Anthropic /v1/messages API rejects a request with an
// orphaned tool_use or tool_result, so a regression here would 400 every long
// conversation through the proxy. The test spans the prune boundary and checks
// the invariant survives ACTUAL rewriting (old results are stubbed).
func TestPruneHistoryPreservesPairingInvariant(t *testing.T) {
	longOutput := strings.Repeat("file content line\n", 50) // >minPruneChars → eligible
	var msgs []json.RawMessage
	want := map[string]bool{}
	for i := 0; i < 14; i++ { // 14 pairs = 28 messages, well past keepRecent+stride
		id := fmt.Sprintf("tu_%d", i)
		want[id] = true
		msgs = append(msgs,
			mustMarshal(map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": id, "name": "Read",
						"input": map[string]any{"path": "x.go"}},
				},
			}),
			mustMarshal(map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": id, "content": longOutput},
				},
			}),
		)
	}

	pruned := PruneHistory(msgs, 4)

	// (1) No message dropped or added — the in-place-rewrite contract.
	if len(pruned) != len(msgs) {
		t.Fatalf("message count changed %d -> %d; pruning must rewrite in place, never drop a message", len(msgs), len(pruned))
	}
	// (2) Every tool_use still has its tool_result and vice versa, set unchanged.
	uses := collectBlockIDs(pruned, "assistant", "tool_use", "id")
	results := collectBlockIDs(pruned, "user", "tool_result", "tool_use_id")
	if !equalStringSet(uses, results) {
		t.Fatalf("tool_use/tool_result pairing broken after prune:\n  uses=%v\n  results=%v", keys(uses), keys(results))
	}
	if !equalStringSet(uses, want) {
		t.Fatalf("tool_use id set changed across prune:\n  want=%v\n  got=%v", keys(want), keys(uses))
	}
	// (3) Non-vacuous: pruning actually happened (old-region results were stubbed),
	// so the invariant is proven to survive REAL rewriting, not a no-op.
	orig := strings.Count(string(mustMarshal(msgs)), "file content line")
	got := strings.Count(string(mustMarshal(pruned)), "file content line")
	if got >= orig {
		t.Fatalf("expected old results stubbed; full-content occurrences not reduced (%d -> %d)", orig, got)
	}
}

// collectBlockIDs gathers the idField of every blockType block in messages of
// the given role.
func collectBlockIDs(msgs []json.RawMessage, role, blockType, idField string) map[string]bool {
	ids := map[string]bool{}
	for _, raw := range msgs {
		var m struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Role != role {
			continue
		}
		for _, blk := range m.Content {
			if t, _ := blk["type"].(string); t != blockType {
				continue
			}
			if id, ok := blk[idField].(string); ok && id != "" {
				ids[id] = true
			}
		}
	}
	return ids
}

func equalStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
