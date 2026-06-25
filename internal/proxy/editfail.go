package proxy

import (
	"encoding/json"
	"strings"
)

// EditFailStats reports edit-fail-after-read events found in a /v1/messages body.
// An event fires when a file is read in the old (prunable) region and a subsequent
// Edit tool call on the same path fails in the recent keep-window.
type EditFailStats struct {
	EditFails int      // count of detected edit-fail events
	Paths     []string // file paths that triggered edit-fail (one entry per event)
}

// editTools is the set of tool names (exact, case-sensitive) Claude Code uses
// for file edits. We match a suffix of the tool name to handle namespaced
// variants (e.g. "mcp__code__Edit").
var editToolSuffixes = []string{"Edit", "str_replace_editor", "ctx_edit"}

// editErrorPhrases are substrings found in Edit tool_result content when the
// edit was not applied. The check is case-insensitive.
var editErrorPhrases = []string{
	"not found",
	"no replacement",
	"no changes",
	"does not match",
	"could not find",
}

// AnalyzeEditFailsBody parses a /v1/messages JSON body and reports edit-fail
// events. Fail-open: returns zero stats on any parse error. Must be called on
// the body before pruning so the old region is still present.
func AnalyzeEditFailsBody(body []byte, keepRecent int) EditFailStats {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return EditFailStats{}
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return EditFailStats{}
	}
	var messages []json.RawMessage
	if json.Unmarshal(msgsRaw, &messages) != nil {
		return EditFailStats{}
	}
	return AnalyzeEditFails(messages, keepRecent)
}

// AnalyzeEditFails detects edit-fail-after-read events in a parsed messages array.
//
// Signal: a file path read by a file-read tool in the OLD region (index < pruneUntil,
// the compression-eligible zone) that is later targeted by an Edit tool call
// returning an error in the RECENT keep-window.
//
// Stateless and fail-open: derived entirely from the request itself, no
// cross-request state, never panics on malformed structure.
func AnalyzeEditFails(messages []json.RawMessage, keepRecent int) EditFailStats {
	if keepRecent < 0 {
		keepRecent = 0
	}
	pruneUntil := len(messages) - keepRecent
	if pruneUntil < 0 {
		pruneUntil = 0
	}

	info := buildToolUseInfo(messages)

	// Collect file paths read in the old (compression-eligible) region.
	readInOld := make(map[string]bool)
	collectFileReads(messages[:pruneUntil], info, func(path, _ string) {
		if path != "" {
			readInOld[path] = true
		}
	})

	// Scan the recent window for Edit tool_results that indicate failure.
	var st EditFailStats
	for _, raw := range messages[pruneUntil:] {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, blkRaw := range blocks {
			var blk map[string]json.RawMessage
			if json.Unmarshal(blkRaw, &blk) != nil {
				continue
			}
			var typ string
			if t, ok := blk["type"]; !ok || json.Unmarshal(t, &typ) != nil || typ != "tool_result" {
				continue
			}
			var id string
			if v, ok := blk["tool_use_id"]; ok {
				_ = json.Unmarshal(v, &id)
			}
			ti := info[id]
			if !isEditTool(ti.name) {
				continue
			}
			if ti.path == "" || !readInOld[ti.path] {
				continue
			}
			// Check whether the tool_result indicates a failure.
			c, ok := blk["content"]
			if !ok {
				continue
			}
			text := extractToolResultText(c)
			if isEditError(text) {
				st.EditFails++
				st.Paths = append(st.Paths, ti.path)
			}
		}
	}
	return st
}

func isEditTool(name string) bool {
	for _, suffix := range editToolSuffixes {
		if name == suffix || strings.HasSuffix(name, "__"+suffix) {
			return true
		}
	}
	return false
}

func isEditError(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range editErrorPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
