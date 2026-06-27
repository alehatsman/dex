package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// Tool classification (IsAskTool / IsConsumeTool) now lives in
// internal/feedback and is tested there.

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

// Real harness form (#734): Claude Code forwards the MCP result with the
// bundle JSON *stringified* inside a content-block text field. The structured
// fields are one decode below the surface — the parser must lift them.
func TestParseAskResponseContentEnvelope(t *testing.T) {
	bundle := `{"intent":"behavior_search","content_bytes_inlined":5787,"suggested_reads":[{"path":"internal/mcp/server.go","start_line":720,"end_line":760},{"path":"cmd/dex/feedback.go"}],"avoid":"Do not read entire files."}`
	env, err := json.Marshal(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": bundle}},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, inlined, intent := parseAskResponse(env)
	if !slices.Contains(paths, "internal/mcp/server.go") || !slices.Contains(paths, "cmd/dex/feedback.go") {
		t.Errorf("stringified-bundle paths not lifted: %v", paths)
	}
	if inlined != 5787 {
		t.Errorf("inlined = %d, want 5787", inlined)
	}
	if intent != "behavior_search" {
		t.Errorf("intent = %q, want behavior_search", intent)
	}
}

// A bare stringified bundle (no envelope) must also resolve.
func TestParseAskResponseBareStringBundle(t *testing.T) {
	bundle := `{"intent":"editing_context","content_bytes_inlined":42,"suggested_reads":[{"path":"a.go"}]}`
	raw, err := json.Marshal(bundle) // a JSON string whose value is itself JSON
	if err != nil {
		t.Fatal(err)
	}
	paths, inlined, intent := parseAskResponse(raw)
	if !slices.Contains(paths, "a.go") || inlined != 42 || intent != "editing_context" {
		t.Errorf("bare stringified bundle not parsed: paths=%v inlined=%d intent=%q", paths, inlined, intent)
	}
}

func TestParseAskResponseEmpty(t *testing.T) {
	paths, inlined, intent := parseAskResponse(nil)
	if paths != nil || inlined != 0 || intent != "" {
		t.Errorf("empty response should be zero-valued, got %v %d %q", paths, inlined, intent)
	}
}
