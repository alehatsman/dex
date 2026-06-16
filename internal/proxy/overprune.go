package proxy

import (
	"encoding/json"

	"github.com/alehatsman/dex/internal/tokens"
)

// ReReadStats is the over-pruning signal for one /v1/messages request: how many
// file reads that PruneHistory stubs in the old region are read AGAIN inside the
// recent keep-window, and the token cost of those re-reads. It is pure
// measurement (#561) — computed alongside pruning, never altering it.
type ReReadStats struct {
	ReReads      int // file reads in the keep-window whose path was stubbed in the old region
	ReReadTokens int // tokens of those re-read results — what re-fetching the stubbed file cost
}

// toolUseInfo captures the bits of an assistant tool_use block the over-pruning
// and dedup passes need: the tool name (for kind classification) and the file
// argument it read (empty for tools that take no path).
type toolUseInfo struct {
	name string
	path string
}

// AnalyzeReReadsBody parses a /v1/messages JSON body and reports its
// re-read-after-stub signal. Fail-open: returns a zero ReReadStats on any parse
// error. Must be called on the ORIGINAL (pre-prune) body — the client always
// re-sends full read content, so both the old and recent copies are present.
func AnalyzeReReadsBody(body []byte, keepRecent int) ReReadStats {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ReReadStats{}
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return ReReadStats{}
	}
	var messages []json.RawMessage
	if json.Unmarshal(msgsRaw, &messages) != nil {
		return ReReadStats{}
	}
	var model string
	if mRaw, ok := raw["model"]; ok {
		_ = json.Unmarshal(mRaw, &model)
	}
	return AnalyzeReReads(messages, keepRecent, tokens.Detect(model))
}

// AnalyzeReReads detects re-read-after-stub events in a parsed messages array.
//
// Signal: a file path read in the OLD region (index < pruneUntil, where
// PruneHistory rewrites the result to a stub) that is read AGAIN inside the
// recent keep-window. The recurrence means the agent paid to re-fetch a file the
// pruner had been discarding — the downside the token-savings counters don't
// show. It mirrors PruneHistory's own gate: only old reads at or above
// minPruneChars actually get stubbed, so only those count toward the signal.
//
// Stateless and fail-open: derived entirely from the request itself, no
// cross-request server state, never panics on malformed structure.
func AnalyzeReReads(messages []json.RawMessage, keepRecent int, fam tokens.Family) ReReadStats {
	if keepRecent < 0 {
		keepRecent = 0
	}
	if len(messages) <= keepRecent {
		return ReReadStats{}
	}
	pruneUntil := len(messages) - keepRecent

	info := buildToolUseInfo(messages)

	// Paths of file reads PruneHistory would stub in the old region (the
	// minPruneChars gate matches rewriteBlock, so we don't flag reads it leaves
	// verbatim).
	stubbed := make(map[string]bool)
	collectFileReads(messages[:pruneUntil], info, func(path, text string) {
		if len(text) >= minPruneChars {
			stubbed[path] = true
		}
	})

	var st ReReadStats
	collectFileReads(messages[pruneUntil:], info, func(path, text string) {
		if stubbed[path] {
			st.ReReads++
			st.ReReadTokens += tokens.CountFor(text, fam)
		}
	})
	return st
}

// buildToolUseInfo maps tool_use_id → {name, read path} by scanning assistant
// tool_use blocks across all messages.
func buildToolUseInfo(messages []json.RawMessage) map[string]toolUseInfo {
	m := make(map[string]toolUseInfo)
	for _, raw := range messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "assistant" {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, blk := range blocks {
			var typ string
			if t, ok := blk["type"]; !ok || json.Unmarshal(t, &typ) != nil || typ != "tool_use" {
				continue
			}
			var id, name string
			if v, ok := blk["id"]; ok {
				_ = json.Unmarshal(v, &id)
			}
			if v, ok := blk["name"]; ok {
				_ = json.Unmarshal(v, &name)
			}
			if id == "" {
				continue
			}
			path := ""
			if v, ok := blk["input"]; ok {
				path = extractReadPath(v)
			}
			m[id] = toolUseInfo{name: name, path: path}
		}
	}
	return m
}

// extractReadPath pulls the file argument out of a tool_use input object,
// trying the common key names across native and dex read tools. Returns ""
// when the input is not an object or carries no recognized path key.
func extractReadPath(input json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "file", "notebook_path", "filename"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// collectFileReads invokes fn(path, text) for every tool_result in msgs that
// resolves (via info) to a file-read tool with a known path. text is the
// flattened result content. Non-user messages, non-tool_result blocks, command
// tools, and path-less reads are skipped.
func collectFileReads(msgs []json.RawMessage, info map[string]toolUseInfo, fn func(path, text string)) {
	for _, raw := range msgs {
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
			if ti.path == "" || classifyTool(ti.name) != kindFileRead {
				continue
			}
			c, ok := blk["content"]
			if !ok {
				continue
			}
			fn(ti.path, extractToolResultText(c))
		}
	}
}
