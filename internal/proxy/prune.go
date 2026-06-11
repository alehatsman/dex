package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultKeepRecent is the number of trailing messages left untouched by
// PruneHistory. Only messages older than this window are candidates for
// tool_result rewriting.
const DefaultKeepRecent = 10

// PruneRequestBody parses an Anthropic /v1/messages JSON body, prunes old
// tool_result blocks via PruneHistory, and returns the rewritten body.
// On any parse or marshal error the original body is returned unchanged
// (fail-open). The second return value is the number of bytes removed (0 if
// pruning was skipped or produced no savings).
func PruneRequestBody(body []byte, keepRecent int) ([]byte, int) {
	var req struct {
		Messages []json.RawMessage          `json:"messages"`
		Rest     map[string]json.RawMessage `json:"-"`
	}

	// Decode into a generic map so we preserve all other fields verbatim.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, 0
	}

	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, 0
	}
	if err := json.Unmarshal(msgsRaw, &req.Messages); err != nil {
		return body, 0
	}

	pruned := PruneHistory(req.Messages, keepRecent)

	newMsgs, err := json.Marshal(pruned)
	if err != nil {
		return body, 0
	}

	// If nothing changed at the messages level, return the original body so we
	// don't perturb key ordering or introduce any other cosmetic diff.
	if string(newMsgs) == string(msgsRaw) {
		return body, 0
	}

	raw["messages"] = newMsgs

	out, err := json.Marshal(raw)
	if err != nil {
		return body, 0
	}
	saved := len(body) - len(out)
	if saved < 0 {
		saved = 0
	}
	return out, saved
}

// PruneHistory rewrites old tool_result blocks in the messages array to shrink
// input-token count. It is deterministic and makes no LLM calls.
//
// Messages within the trailing keepRecent window are left untouched. For older
// messages the function classifies each tool_result by the tool that produced
// it and replaces bulky content with a compact stub:
//
//   - file/source reads → honest re-read stub (no partial excerpt — that misleads)
//   - command/log/search output → head/tail summary (first 3 + last 2 lines)
//
// Content shorter than minPruneChars is left as-is (already cheap). Malformed
// messages are left untouched (fail-open). The returned slice shares no backing
// arrays with the input.
func PruneHistory(messages []json.RawMessage, keepRecent int) []json.RawMessage {
	if keepRecent < 0 {
		keepRecent = 0
	}
	if len(messages) <= keepRecent {
		return messages
	}

	// Pass 1: build tool_use_id → tool_name from ALL messages (assistant blocks).
	toolNames := buildToolNameMap(messages)

	out := make([]json.RawMessage, len(messages))
	pruneUntil := len(messages) - keepRecent

	for i, raw := range messages {
		if i >= pruneUntil {
			out[i] = raw
			continue
		}
		rewritten, ok := rewriteMessage(raw, toolNames)
		if !ok {
			out[i] = raw // fail-open
			continue
		}
		out[i] = rewritten
	}
	return out
}

// minPruneChars: content shorter than this is left as-is.
const minPruneChars = 200

// buildToolNameMap scans all messages for assistant tool_use blocks and
// returns a mapping tool_use_id → tool_name.
func buildToolNameMap(messages []json.RawMessage) map[string]string {
	m := make(map[string]string)
	for _, raw := range messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Role != "assistant" {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, blk := range blocks {
			typeRaw, ok := blk["type"]
			if !ok {
				continue
			}
			var typ string
			if json.Unmarshal(typeRaw, &typ) != nil || typ != "tool_use" {
				continue
			}
			var id, name string
			if idRaw, ok := blk["id"]; ok {
				_ = json.Unmarshal(idRaw, &id)
			}
			if nameRaw, ok := blk["name"]; ok {
				_ = json.Unmarshal(nameRaw, &name)
			}
			if id != "" && name != "" {
				m[id] = name
			}
		}
	}
	return m
}

// rewriteMessage rewrites tool_result blocks in a single message.
// Returns (rewritten, true) on success; (nil, false) if the message should be
// left untouched (fail-open).
func rewriteMessage(raw json.RawMessage, toolNames map[string]string) (json.RawMessage, bool) {
	// Decode into a generic map to preserve unknown fields.
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil, false
	}
	roleRaw, ok := msg["role"]
	if !ok {
		return nil, false
	}
	var role string
	if json.Unmarshal(roleRaw, &role) != nil {
		return nil, false
	}
	// Only user messages carry tool_result blocks.
	if role != "user" {
		return raw, true
	}

	contentRaw, ok := msg["content"]
	if !ok {
		return raw, true
	}

	// Content can be a bare string (no tool_results possible) or array.
	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return raw, true // bare string — nothing to prune
	}

	changed := false
	newBlocks := make([]json.RawMessage, len(blocks))
	for i, blkRaw := range blocks {
		rewritten, didChange := rewriteBlock(blkRaw, toolNames)
		newBlocks[i] = rewritten
		if didChange {
			changed = true
		}
	}
	if !changed {
		return raw, true
	}

	newContent, err := json.Marshal(newBlocks)
	if err != nil {
		return nil, false
	}
	msg["content"] = newContent
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteBlock rewrites one content block if it is a tool_result eligible for
// pruning. Returns the (possibly rewritten) block and whether it changed.
func rewriteBlock(raw json.RawMessage, toolNames map[string]string) (json.RawMessage, bool) {
	var blk map[string]json.RawMessage
	if json.Unmarshal(raw, &blk) != nil {
		return raw, false
	}
	typeRaw, ok := blk["type"]
	if !ok {
		return raw, false
	}
	var typ string
	if json.Unmarshal(typeRaw, &typ) != nil || typ != "tool_result" {
		return raw, false
	}

	// Resolve tool name.
	var toolUseID string
	if idRaw, ok := blk["tool_use_id"]; ok {
		_ = json.Unmarshal(idRaw, &toolUseID)
	}
	toolName := toolNames[toolUseID] // empty string if unknown

	// Extract the full text of this tool_result for length check + rewrite.
	contentRaw, hasContent := blk["content"]
	if !hasContent {
		return raw, false
	}
	text := extractToolResultText(contentRaw)
	if len(text) < minPruneChars {
		return raw, false // already short
	}

	kind := classifyTool(toolName)
	var stub string
	switch kind {
	case kindFileRead:
		lines := countLines(text)
		stub = fmt.Sprintf("[earlier file read (%d lines) pruned to save tokens; re-read if needed]", lines)
	case kindCommand:
		stub = headTailSummary(text)
	default:
		// Unknown tool — use command-style summary (conservative).
		stub = headTailSummary(text)
	}

	// Replace content with the stub string.
	newContent, err := json.Marshal(stub)
	if err != nil {
		return raw, false
	}
	blk["content"] = newContent
	out, err := json.Marshal(blk)
	if err != nil {
		return raw, false
	}
	return out, true
}

// extractToolResultText flattens tool_result content (string or array-of-text-blocks)
// into a single string for analysis.
func extractToolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if txtRaw, ok := blk["text"]; ok {
			var t string
			if json.Unmarshal(txtRaw, &t) == nil {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// toolKind classifies a tool by name for pruning purposes.
type toolKind int

const (
	kindUnknown  toolKind = iota
	kindFileRead          // file/source content — use re-read stub
	kindCommand           // shell/log/search output — use head/tail summary
)

// fileReadTools is the set of tool-name substrings that indicate a file-read.
// Matching is case-insensitive substring, same heuristic as lean-ctx tool_kind.rs.
var fileReadKeywords = []string{
	"read", "view", "cat", "open",
}

// commandKeywords identifies shell/log/search tools.
var commandKeywords = []string{
	"bash", "shell", "exec", "run", "cmd", "search", "grep",
	"find", "glob", "computer",
}

func classifyTool(name string) toolKind {
	lower := strings.ToLower(name)
	for _, kw := range fileReadKeywords {
		if strings.Contains(lower, kw) {
			return kindFileRead
		}
	}
	for _, kw := range commandKeywords {
		if strings.Contains(lower, kw) {
			return kindCommand
		}
	}
	return kindUnknown
}

// countLines counts newline-delimited lines in s (at least 1 for non-empty).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// headTailSummary returns the first 3 lines + last 2 lines of s, with a pruned
// count in between. If there are ≤5 lines the full text is returned.
func headTailSummary(s string) string {
	lines := strings.Split(s, "\n")
	// Trim trailing empty line left by a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	const head, tail = 3, 2
	if len(lines) <= head+tail {
		return s
	}
	pruned := len(lines) - head - tail
	headLines := strings.Join(lines[:head], "\n")
	tailLines := strings.Join(lines[len(lines)-tail:], "\n")
	return fmt.Sprintf("%s\n... (%d lines pruned)\n%s", headLines, pruned, tailLines)
}
