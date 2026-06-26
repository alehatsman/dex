package main

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestIsAskAndConsumeTools(t *testing.T) {
	if !isAskTool("mcp__dex__ask") || !isAskTool("ask") {
		t.Error("ask tools not recognized")
	}
	if isAskTool("Read") || isAskTool("mcp__dex__find") {
		t.Error("non-ask tool misclassified as ask")
	}
	for _, c := range []string{"Read", "Edit", "Write", "NotebookEdit", "mcp__dex__read"} {
		if !isConsumeTool(c) {
			t.Errorf("%q should be a consume tool", c)
		}
	}
	if isConsumeTool("Bash") || isConsumeTool("mcp__dex__ask") {
		t.Error("non-consume tool misclassified")
	}
}

func TestPathsFromInput(t *testing.T) {
	got := pathsFromInput(json.RawMessage(`{"file_path":"/abs/x.go","other":1}`))
	if !slices.Equal(got, []string{"/abs/x.go"}) {
		t.Errorf("file_path: got %v", got)
	}
	got = pathsFromInput(json.RawMessage(`{"path":"internal/y.go"}`))
	if !slices.Equal(got, []string{"internal/y.go"}) {
		t.Errorf("path: got %v", got)
	}
	if pathsFromInput(nil) != nil || pathsFromInput(json.RawMessage(`not json`)) != nil {
		t.Error("malformed/empty input should yield nil")
	}
}

// Structured-content form: suggested_reads array with path fields.
func TestParseAskResponseStructured(t *testing.T) {
	raw := json.RawMessage(`{"suggested_reads":[{"path":"a/b.go","start_line":1,"end_line":5},{"path":"c/d.go"}],"content_bytes_inlined":1234,"intent":"assemble"}`)
	paths, inlined, intent := parseAskResponse(raw)
	if !slices.Contains(paths, "a/b.go") || !slices.Contains(paths, "c/d.go") {
		t.Errorf("paths missing: %v", paths)
	}
	if inlined != 1234 {
		t.Errorf("inlined = %d, want 1234", inlined)
	}
	if intent != "assemble" {
		t.Errorf("intent = %q, want assemble", intent)
	}
}

// Wrapped form: the array sits under a structuredContent envelope.
func TestParseAskResponseWrapped(t *testing.T) {
	raw := json.RawMessage(`{"structuredContent":{"suggested_reads":[{"path":"x.go"}]}}`)
	paths, _, _ := parseAskResponse(raw)
	if !slices.Contains(paths, "x.go") {
		t.Errorf("wrapped suggested_reads not found: %v", paths)
	}
}

// Text-rendering form: content blocks carry the formatted bundle, paths as
// "file:line-line" tokens.
func TestParseAskResponseTextForm(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"1. internal/proxy/prune.go:232-251\n  intent: assemble\n  content_bytes_inlined: 999\n"}]}`)
	paths, inlined, intent := parseAskResponse(raw)
	if !slices.Contains(paths, "internal/proxy/prune.go") {
		t.Errorf("text-form path not extracted: %v", paths)
	}
	if inlined != 999 {
		t.Errorf("inlined = %d, want 999", inlined)
	}
	if intent != "assemble" {
		t.Errorf("intent = %q, want assemble", intent)
	}
}

func TestParseAskResponseEmpty(t *testing.T) {
	paths, inlined, intent := parseAskResponse(nil)
	if paths != nil || inlined != 0 || intent != "" {
		t.Errorf("empty response should be zero-valued, got %v %d %q", paths, inlined, intent)
	}
}
