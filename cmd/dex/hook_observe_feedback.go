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
// is wrapped: it walks the decoded JSON for any nested suggested_reads array
// (the structured-content form) AND scans the raw bytes for path:line-line
// tokens (the text-rendering form), unioning both so it works whether the
// harness forwards structured content, text, or both.
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
		for _, p := range collectSuggestedReadPaths(root) {
			add(p)
		}
	}
	for _, m := range askPathLineRe.FindAllStringSubmatch(string(raw), -1) {
		add(m[1])
	}
	if m := askInlinedRe.FindStringSubmatch(string(raw)); m != nil {
		inlined, _ = strconv.Atoi(m[1])
	}
	if m := askIntentRe.FindStringSubmatch(string(raw)); m != nil {
		intent = m[1]
	}
	return paths, inlined, intent
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
