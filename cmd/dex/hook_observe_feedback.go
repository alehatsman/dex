package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the observe-event enrichment that the `dex feedback`
// consumer joins on (#724). Two sides of one join:
//
//   - a read/edit tool records the file it consumed (pathsFromInput);
//   - an ask call records the reads it recommended, plus the bytes it
//     inlined and the intent it routed to (parseAskResponse).
//
// Both are best-effort and fail soft: a shape we don't recognize yields no
// paths rather than an error, so the hook never blocks a tool call.

// isAskTool reports whether name is a dex `ask` call (the MCP tool is
// mcp__dex__ask; the bare name covers a future rename or a direct caller).
func isAskTool(name string) bool {
	return name == "ask" || strings.HasSuffix(name, "__ask")
}

// isConsumeTool reports whether name is a tool that opens a file by path —
// the consumption side of the suggested-read join.
func isConsumeTool(name string) bool {
	switch name {
	case "Read", "Edit", "MultiEdit", "Write", "NotebookEdit":
		return true
	}
	return strings.HasSuffix(name, "__read") // mcp__dex__read
}

// pathsFromInput pulls the file path(s) a consume tool targeted out of its
// tool_input. Read/Edit/Write use file_path, NotebookEdit notebook_path,
// dex read uses path.
func pathsFromInput(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var ti struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(raw, &ti); err != nil {
		return nil
	}
	var out []string
	for _, p := range []string{ti.FilePath, ti.Path, ti.NotebookPath} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var (
	// path with extension followed by a :start-end line range, as the ask
	// bundle renders suggested_reads in text ("internal/proxy/prune.go:232-251").
	askPathLineRe = regexp.MustCompile(`([A-Za-z0-9_./\-]+\.[A-Za-z0-9]+):\d+-\d+`)
	askInlinedRe  = regexp.MustCompile(`content_bytes_inlined["\s:]+(\d+)`)
	askIntentRe   = regexp.MustCompile(`intent["\s:]+"?([a-z_]+)`)
)

// parseAskInput extracts the question text from an ask tool_input so the
// observer can record it for curated-golden miss-mining (#732).
func parseAskInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var ti struct {
		Question string `json:"question"`
	}
	if json.Unmarshal(raw, &ti) != nil {
		return ""
	}
	return ti.Question
}

// parseAskResponse extracts the suggested-read paths, inlined byte count, and
// routed intent from an ask tool_response. It is tolerant of how the response
// is wrapped. The real harness (Claude Code) forwards the MCP result with the
// bundle JSON *stringified* inside a content envelope —
// {"content":[{"type":"text","text":"{...bundle...}"}]} — so the structured
// fields sit one decode below the surface; resolveJSONStrings lifts any
// JSON-in-a-string back into the tree before the walk, then the fields are
// read structurally. Without that lift the walk never reaches suggested_reads
// and the metric reads 0 even though every ask carried a full bundle (#734).
// The raw-byte regexes remain as a last-resort fallback for a harness that
// forwards a flat text rendering with no structured bundle at all.
func parseAskResponse(raw json.RawMessage) (paths []string, inlined int, intent string) {
	if len(raw) == 0 {
		return nil, 0, ""
	}
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	var root any
	if json.Unmarshal(raw, &root) == nil {
		root = resolveJSONStrings(root)
		for _, p := range collectSuggestedReadPaths(root) {
			add(p)
		}
		inlined, intent = findAskScalars(root)
	}

	// Flat-text fallbacks: only when the structured walk found nothing, so a
	// stringified bundle never reaches the loose regexes (which mismatch the
	// escaped form anyway and would false-match e.g. intent="it" on prose).
	if len(paths) == 0 {
		for _, m := range askPathLineRe.FindAllStringSubmatch(string(raw), -1) {
			add(m[1])
		}
	}
	if inlined == 0 {
		if m := askInlinedRe.FindStringSubmatch(string(raw)); m != nil {
			inlined, _ = strconv.Atoi(m[1])
		}
	}
	if intent == "" {
		if m := askIntentRe.FindStringSubmatch(string(raw)); m != nil {
			intent = m[1]
		}
	}
	return paths, inlined, intent
}

// resolveJSONStrings returns v with every string value that is itself a JSON
// object or array re-parsed in place, recursively. Claude Code stringifies the
// ask bundle into a content-block text field, so the structured payload is one
// (or more) JSON decodes below the surface; lifting it lets the path/scalar
// walks see suggested_reads, content_bytes_inlined and intent (#734).
func resolveJSONStrings(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = resolveJSONStrings(val)
		}
		return t
	case []any:
		for i, it := range t {
			t[i] = resolveJSONStrings(it)
		}
		return t
	case string:
		s := strings.TrimSpace(t)
		if len(s) > 1 && (s[0] == '{' || s[0] == '[') {
			var inner any
			if json.Unmarshal([]byte(s), &inner) == nil {
				return resolveJSONStrings(inner)
			}
		}
		return t
	default:
		return t
	}
}

// findAskScalars walks a resolved JSON tree for the first content_bytes_inlined
// and intent values at any depth — read structurally so the escaped-bytes
// regexes (which the stringified bundle defeats) are not relied on (#734).
func findAskScalars(v any) (inlined int, intent string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch {
			case k == "content_bytes_inlined" && inlined == 0:
				if f, ok := val.(float64); ok {
					inlined = int(f)
				}
			case k == "intent" && intent == "":
				if s, ok := val.(string); ok {
					intent = s
				}
			}
			ci, cs := findAskScalars(val)
			if inlined == 0 {
				inlined = ci
			}
			if intent == "" {
				intent = cs
			}
		}
	case []any:
		for _, it := range t {
			ci, cs := findAskScalars(it)
			if inlined == 0 {
				inlined = ci
			}
			if intent == "" {
				intent = cs
			}
		}
	}
	return inlined, intent
}

// collectSuggestedReadPaths walks a decoded JSON value and returns the `path`
// field of every object inside any `suggested_reads` array, at any nesting
// depth — the array may sit under MCP content / structuredContent wrappers.
func collectSuggestedReadPaths(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "suggested_reads" {
				if arr, ok := val.([]any); ok {
					for _, it := range arr {
						if m, ok := it.(map[string]any); ok {
							if p, ok := m["path"].(string); ok {
								out = append(out, p)
							}
						}
					}
				}
			}
			out = append(out, collectSuggestedReadPaths(val)...)
		}
	case []any:
		for _, it := range t {
			out = append(out, collectSuggestedReadPaths(it)...)
		}
	}
	return out
}
