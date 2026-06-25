package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// toolsBody builds a minimal /v1/messages body carrying the given tools.
func toolsBody(t *testing.T, tools []map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"model": "claude-opus-4-8",
		"tools": tools,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

// parseTools pulls the tools array back out of a request body for assertions.
func parseTools(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(root["tools"], &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	return tools
}

func toolField(t *testing.T, tool map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(tool[key], &s); err != nil {
		t.Fatalf("unmarshal field %q: %v", key, err)
	}
	return s
}

func TestParseToolDescMode(t *testing.T) {
	cases := map[string]ToolDescMode{
		"":        ToolDescFull,
		"full":    ToolDescFull,
		"FULL":    ToolDescFull,
		"  terse": ToolDescTerse,
		"Terse":   ToolDescTerse,
		"lazy":    ToolDescLazy,
		"bogus":   ToolDescFull, // unknown → safe default
	}
	for in, want := range cases {
		if got := ParseToolDescMode(in); got != want {
			t.Errorf("ParseToolDescMode(%q) = %v, want %v", in, got, want)
		}
	}
}

// Full mode is a no-op and must return the original bytes unchanged.
func TestCompressToolDescriptionsFullIsNoop(t *testing.T) {
	body := toolsBody(t, []map[string]any{
		{"name": "search", "description": "Line one.\nExample: foo\nLine two.", "input_schema": map[string]any{"type": "object"}},
	})
	out, stats := CompressToolDescriptions(body, ToolDescFull)
	if stats.Applied {
		t.Fatal("Full mode must not apply")
	}
	if string(out) != string(body) {
		t.Fatal("Full mode must return body unchanged")
	}
}

// Terse drops Example/Note/See-also lines, caps at 3, and never touches name
// or input_schema.
func TestCompressToolDescriptionsTerse(t *testing.T) {
	desc := "First substantive line.\n\nExample: do not keep me\nNote: drop me too\nSecond line.\nThird line.\nFourth line should be cut."
	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	body := toolsBody(t, []map[string]any{
		{"name": "search_semantic", "description": desc, "input_schema": schema},
	})

	out, stats := CompressToolDescriptions(body, ToolDescTerse)
	if !stats.Applied || stats.ToolsCompressed != 1 {
		t.Fatalf("expected 1 tool compressed, got applied=%v n=%d", stats.Applied, stats.ToolsCompressed)
	}

	tools := parseTools(t, out)
	got := toolField(t, tools[0], "description")
	lines := strings.Split(got, "\n")
	if len(lines) > 3 {
		t.Fatalf("terse must keep ≤3 lines, got %d: %q", len(lines), got)
	}
	if strings.Contains(got, "Example") || strings.Contains(got, "Note:") {
		t.Fatalf("terse must drop Example/Note lines, got %q", got)
	}
	if toolField(t, tools[0], "name") != "search_semantic" {
		t.Fatal("terse must not touch name")
	}
	// input_schema must be byte-identical.
	wantSchema, _ := json.Marshal(schema)
	if string(tools[0]["input_schema"]) != string(wantSchema) {
		t.Fatalf("terse must not touch input_schema:\n got %s\nwant %s", tools[0]["input_schema"], wantSchema)
	}
}

// Lazy keeps only a truncated first line plus the pointer suffix.
func TestCompressToolDescriptionsLazy(t *testing.T) {
	long := strings.Repeat("x", 200)
	desc := long + "\nsecond line we must drop"
	body := toolsBody(t, []map[string]any{
		{"name": "graph_callers", "description": desc, "input_schema": map[string]any{"type": "object"}},
	})

	out, stats := CompressToolDescriptions(body, ToolDescLazy)
	if !stats.Applied {
		t.Fatal("lazy must apply")
	}
	got := toolField(t, parseTools(t, out)[0], "description")
	if strings.Contains(got, "second line") {
		t.Fatalf("lazy must keep only the first line, got %q", got)
	}
	if !strings.HasSuffix(got, lazyPointerSuffix) {
		t.Fatalf("lazy must append the pointer suffix, got %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("lazy must mark truncation with an ellipsis, got %q", got)
	}
	// Truncated line is at most lazyMaxRunes runes + ellipsis before the suffix.
	head := strings.TrimSuffix(got, lazyPointerSuffix)
	head = strings.TrimSuffix(head, "…")
	if n := len([]rune(head)); n > lazyMaxRunes {
		t.Fatalf("lazy head = %d runes, want ≤%d", n, lazyMaxRunes)
	}
}

// Lazy truncation must split on a rune boundary, never a byte mid-codepoint.
func TestCompressToolDescriptionsLazyRuneSafe(t *testing.T) {
	desc := strings.Repeat("é", 100) // 2 bytes each — byte-truncation would corrupt
	body := toolsBody(t, []map[string]any{
		{"name": "t", "description": desc, "input_schema": map[string]any{}},
	})
	out, _ := CompressToolDescriptions(body, ToolDescLazy)
	got := toolField(t, parseTools(t, out)[0], "description")
	if !json.Valid(out) {
		t.Fatal("output must remain valid JSON")
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("rune-unsafe truncation produced U+FFFD: %q", got)
		}
	}
}

// A tool with no description is left alone; a body with no tools is a no-op.
func TestCompressToolDescriptionsSkips(t *testing.T) {
	// No description field.
	body := toolsBody(t, []map[string]any{
		{"name": "x", "input_schema": map[string]any{}},
	})
	if _, stats := CompressToolDescriptions(body, ToolDescLazy); stats.Applied {
		t.Fatal("tool without description must not count as compressed")
	}

	// No tools array.
	noTools, _ := json.Marshal(map[string]any{"model": "claude-opus-4-8", "messages": []any{}})
	out, stats := CompressToolDescriptions(noTools, ToolDescTerse)
	if stats.Applied || string(out) != string(noTools) {
		t.Fatal("body without tools must be a no-op")
	}
}

// Malformed input must fail open: original bytes back, zero stats.
func TestCompressToolDescriptionsFailOpen(t *testing.T) {
	bad := []byte(`{"tools": not json`)
	out, stats := CompressToolDescriptions(bad, ToolDescLazy)
	if stats.Applied {
		t.Fatal("malformed body must not report Applied")
	}
	if string(out) != string(bad) {
		t.Fatal("malformed body must be returned unchanged")
	}

	if out, stats := CompressToolDescriptions(nil, ToolDescTerse); stats.Applied || out != nil {
		t.Fatal("nil body must be a no-op")
	}
}

// End-to-end: a request through the real handler in Lazy mode must reach
// upstream with descriptions compressed but names/schemas intact, and the
// per-request log must carry the tool-desc fields.
func TestToolDescCompressionIntegration(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(up.Close)
	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	front := httptest.NewServer(newProxyHandler(upURL, logger, &Stats{}, "", ToolDescLazy, ModelRouteConfig{}, nil, nil, nil, nil, "", nil))
	t.Cleanup(front.Close)

	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	body := toolsBody(t, []map[string]any{
		{"name": "search_semantic", "description": strings.Repeat("z", 200) + "\nsecond line", "input_schema": schema},
	})

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	tools := parseTools(t, gotBody)
	if got := toolField(t, tools[0], "name"); got != "search_semantic" {
		t.Fatalf("upstream name mutated: %q", got)
	}
	wantSchema, _ := json.Marshal(schema)
	if string(tools[0]["input_schema"]) != string(wantSchema) {
		t.Fatalf("upstream schema mutated:\n got %s\nwant %s", tools[0]["input_schema"], wantSchema)
	}
	desc := toolField(t, tools[0], "description")
	if !strings.HasSuffix(desc, lazyPointerSuffix) || strings.Contains(desc, "second line") {
		t.Fatalf("upstream description not lazy-compressed: %q", desc)
	}
	if !strings.Contains(logs.String(), "tool_descs_compressed=1") {
		t.Fatalf("log missing tool_descs_compressed=1:\n%s", logs.String())
	}
}

// Determinism: the same tools block compresses to identical bytes every call —
// the invariant the cache-alignment pass depends on for cross-turn cache hits.
func TestCompressToolDescriptionsDeterministic(t *testing.T) {
	body := toolsBody(t, []map[string]any{
		{"name": "a", "description": "Alpha line.\nExample: x\nBeta line.", "input_schema": map[string]any{"type": "object"}},
		{"name": "b", "description": strings.Repeat("y", 120), "input_schema": map[string]any{"type": "object"}},
	})
	first, _ := CompressToolDescriptions(body, ToolDescTerse)
	second, _ := CompressToolDescriptions(body, ToolDescTerse)
	if string(first) != string(second) {
		t.Fatal("tool-desc compression must be deterministic across calls")
	}
}
