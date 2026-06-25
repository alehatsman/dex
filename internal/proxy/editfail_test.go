package proxy

import (
	"encoding/json"
	"fmt"
	"testing"
)

// buildMessages creates a simple messages array for test use.
// Each entry is a raw JSON object representing a message.
func editFailMessages(msgs []map[string]any) []json.RawMessage {
	out := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, _ := json.Marshal(m)
		out[i] = b
	}
	return out
}

// assistantToolUse returns an assistant message with a single tool_use block.
func assistantToolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "tool_use",
				"id":    id,
				"name":  name,
				"input": input,
			},
		},
	}
}

// userToolResult returns a user message with a single tool_result block.
func userToolResult(id, content string) map[string]any {
	return map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     content,
			},
		},
	}
}

func TestAnalyzeEditFails_NoEvents(t *testing.T) {
	// No messages → no edit fails.
	st := AnalyzeEditFails(nil, 10)
	if st.EditFails != 0 {
		t.Errorf("expected 0 edit fails, got %d", st.EditFails)
	}
}

func TestAnalyzeEditFails_EditSuccessNotCounted(t *testing.T) {
	// Old region: file read; new region: Edit succeeds.
	msgs := editFailMessages([]map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "foo.go"}),
		userToolResult("r1", "package main\n\nfunc main() {}"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "foo.go", "old_string": "a", "new_string": "b"}),
		userToolResult("e1", "The file /path/foo.go has been edited successfully."),
	})
	st := AnalyzeEditFails(msgs, 2) // keepRecent=2 → old region is first 2
	if st.EditFails != 0 {
		t.Errorf("successful edit should not count; got %d", st.EditFails)
	}
}

func TestAnalyzeEditFails_EditFailAfterCompressedRead(t *testing.T) {
	// Old region: file read; new region: Edit fails with "not found".
	msgs := editFailMessages([]map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "internal/foo.go"}),
		userToolResult("r1", "package foo\n\nfunc Foo() {}"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "internal/foo.go", "old_string": "old", "new_string": "new"}),
		userToolResult("e1", "The `old_string` parameter (1 occurrence) was not found in internal/foo.go."),
	})
	// keepRecent=2 → messages[0:2] are old region, messages[2:4] are recent.
	st := AnalyzeEditFails(msgs, 2)
	if st.EditFails != 1 {
		t.Errorf("expected 1 edit fail, got %d", st.EditFails)
	}
	if len(st.Paths) != 1 || st.Paths[0] != "internal/foo.go" {
		t.Errorf("expected path internal/foo.go, got %v", st.Paths)
	}
}

func TestAnalyzeEditFails_EditFailOnDifferentFile(t *testing.T) {
	// Read bar.go in old region; Edit on baz.go fails — should NOT count.
	msgs := editFailMessages([]map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "bar.go"}),
		userToolResult("r1", "package bar"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "baz.go", "old_string": "x", "new_string": "y"}),
		userToolResult("e1", "The `old_string` was not found in baz.go."),
	})
	st := AnalyzeEditFails(msgs, 2)
	if st.EditFails != 0 {
		t.Errorf("edit on different file should not count; got %d", st.EditFails)
	}
}

func TestAnalyzeEditFails_MultipleEvents(t *testing.T) {
	// Two separate files: read in old region, each edit fails.
	msgs := editFailMessages([]map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "a.go"}),
		userToolResult("r1", "content a"),
		assistantToolUse("r2", "mcp__dex__read", map[string]any{"path": "b.go"}),
		userToolResult("r2", "content b"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "a.go", "old_string": "x", "new_string": "y"}),
		userToolResult("e1", "not found"),
		assistantToolUse("e2", "Edit", map[string]any{"file_path": "b.go", "old_string": "p", "new_string": "q"}),
		userToolResult("e2", "No replacement was performed."),
	})
	// keepRecent=4 → messages[0:4] are old, messages[4:8] are recent.
	st := AnalyzeEditFails(msgs, 4)
	if st.EditFails != 2 {
		t.Errorf("expected 2 edit fails, got %d", st.EditFails)
	}
}

func TestAnalyzeEditFailsBody_FailOpen(t *testing.T) {
	// Malformed JSON → zero stats, no panic.
	st := AnalyzeEditFailsBody([]byte("not json"), 10)
	if st.EditFails != 0 {
		t.Errorf("expected zero stats for bad input, got %d", st.EditFails)
	}
}

func TestAnalyzeEditFails_AllInKeepWindow(t *testing.T) {
	// Everything in the keep window → old region is empty → no events.
	msgs := editFailMessages([]map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "foo.go"}),
		userToolResult("r1", "content"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "foo.go", "old_string": "x", "new_string": "y"}),
		userToolResult("e1", "was not found"),
	})
	// keepRecent == total messages → no old region.
	st := AnalyzeEditFails(msgs, len(msgs))
	if st.EditFails != 0 {
		t.Errorf("nothing in old region → no events; got %d", st.EditFails)
	}
}

func TestAnalyzeEditFails_IsEditTool(t *testing.T) {
	cases := []struct {
		name   string
		isEdit bool
	}{
		{"Edit", true},
		{"str_replace_editor", true},
		{"ctx_edit", true},
		{"mcp__code__Edit", true},
		{"mcp__dex__read", false},
		{"Bash", false},
		{"", false},
	}
	for _, c := range cases {
		got := isEditTool(c.name)
		if got != c.isEdit {
			t.Errorf("isEditTool(%q) = %v, want %v", c.name, got, c.isEdit)
		}
	}
}

func TestAnalyzeEditFails_IsEditError(t *testing.T) {
	cases := []struct {
		text    string
		isError bool
	}{
		{"The `old_string` parameter was not found in foo.go.", true},
		{"No replacement was performed.", true},
		{"No changes were made.", true},
		{"The file has been edited successfully.", false},
		{"Successfully updated file.", false},
	}
	for _, c := range cases {
		got := isEditError(c.text)
		if got != c.isError {
			t.Errorf("isEditError(%q) = %v, want %v", c.text, got, c.isError)
		}
	}
}

func TestAnalyzeEditFailsBody_ValidBody(t *testing.T) {
	msgs := []map[string]any{
		assistantToolUse("r1", "mcp__dex__read", map[string]any{"path": "main.go"}),
		userToolResult("r1", "package main"),
		assistantToolUse("e1", "Edit", map[string]any{"file_path": "main.go", "old_string": "x", "new_string": "y"}),
		userToolResult("e1", "old_string not found"),
	}
	msgsJSON, _ := json.Marshal(msgs)
	body := fmt.Sprintf(`{"messages": %s}`, msgsJSON)
	st := AnalyzeEditFailsBody([]byte(body), 2)
	if st.EditFails != 1 {
		t.Errorf("expected 1 edit fail from body, got %d", st.EditFails)
	}
}
