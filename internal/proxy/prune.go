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

// PruneStride is exported so callers can reason about worst-case cache misses.
// The prune boundary is rounded down to the nearest multiple of this stride so
// the byte prefix stays identical between jumps, keeping the provider cache warm.
const PruneStride = 16

// pruneStart returns the first message index eligible for rewriting.
// Rounds down to the nearest PruneStride multiple so the byte prefix
// stays identical between jumps, keeping the provider cache warm.
func pruneStart(msgLen, keepRecent int) int {
	raw := msgLen - keepRecent
	if raw <= 0 {
		return 0
	}
	return (raw / PruneStride) * PruneStride
}

// hasCacheControlMarker reports whether a single raw message JSON contains a
// cache_control key at any of the three nesting levels Anthropic supports:
//  1. Top-level message object
//  2. Content block inside message.content[]
//  3. Text span inside a content block
func hasCacheControlMarker(raw json.RawMessage) bool {
	var msg map[string]any
	if json.Unmarshal(raw, &msg) != nil {
		return false
	}
	if _, ok := msg["cache_control"]; ok {
		return true
	}
	contentAny, ok := msg["content"]
	if !ok {
		return false
	}
	blocks, ok := contentAny.([]any)
	if !ok {
		return false
	}
	for _, blkAny := range blocks {
		blk, ok := blkAny.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := blk["cache_control"]; ok {
			return true
		}
		// Check text spans inside the block (level 3).
		if spansAny, ok := blk["content"]; ok {
			if spans, ok := spansAny.([]any); ok {
				for _, spanAny := range spans {
					if span, ok := spanAny.(map[string]any); ok {
						if _, ok := span["cache_control"]; ok {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// cachedPrefixLen returns the index after the last cache_control marker,
// scanning at message, content-block, and text levels.
// Returns 0 if no marker found (prune freely).
func cachedPrefixLen(messages []json.RawMessage) int {
	last := 0
	for i, raw := range messages {
		if hasCacheControlMarker(raw) {
			last = i + 1
		}
	}
	return last
}

// PruneRequestBody parses an Anthropic /v1/messages JSON body, prunes old
// tool_result blocks via PruneHistory, and returns the rewritten body.
// On any parse or marshal error the original body is returned unchanged
// (fail-open). The second return value is the number of bytes removed (0 if
// pruning was skipped or produced no savings). The third is the prune stats.
func PruneRequestBody(body []byte, keepRecent int, store *TeeStore) ([]byte, int, PruneHistoryStats) {
	var req struct {
		Messages []json.RawMessage          `json:"messages"`
		Rest     map[string]json.RawMessage `json:"-"`
	}

	// Decode into a generic map so we preserve all other fields verbatim.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, 0, PruneHistoryStats{}
	}

	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, 0, PruneHistoryStats{}
	}
	if err := json.Unmarshal(msgsRaw, &req.Messages); err != nil {
		return body, 0, PruneHistoryStats{}
	}

	pruned, pruneSt := pruneHistory(req.Messages, keepRecent, store)

	newMsgs, err := json.Marshal(pruned)
	if err != nil {
		return body, 0, PruneHistoryStats{}
	}

	// If nothing changed at the messages level, return the original body so we
	// don't perturb key ordering or introduce any other cosmetic diff.
	if string(newMsgs) == string(msgsRaw) {
		return body, 0, pruneSt
	}

	raw["messages"] = newMsgs

	out, err := json.Marshal(raw)
	if err != nil {
		return body, 0, PruneHistoryStats{}
	}
	saved := len(body) - len(out)
	if saved < 0 {
		saved = 0
	}
	return out, saved, pruneSt
}

// PruneHistoryStats accumulates result-preservation counters from one
// PruneHistory pass.
type PruneHistoryStats struct {
	// ResultsPreserved is the count of tool_result blocks that were kept
	// verbatim because they were already compressed (dex saved_pct > 0) or
	// carried an <lc_safe> marker — blocks that would otherwise have been
	// replaced with a re-read stub or head/tail summary.
	ResultsPreserved int
	// TokensPreserved is the rough token count of the preserved content
	// (len(text)/4), summed across all preserved results.
	TokensPreserved int
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
//   - already-compressed dex results (saved_pct > 0) → kept verbatim
//   - <lc_safe>-marked content → kept verbatim
//   - test/build output → kept verbatim (error context is load-bearing)
//
// Content shorter than minPruneChars is left as-is (already cheap). Malformed
// messages are left untouched (fail-open). The returned slice shares no backing
// arrays with the input.
func PruneHistory(messages []json.RawMessage, keepRecent int) []json.RawMessage {
	out, _ := pruneHistory(messages, keepRecent, nil)
	return out
}

// PruneHistoryWithStats is like PruneHistory but also returns rewrite statistics.
func PruneHistoryWithStats(messages []json.RawMessage, keepRecent int) ([]json.RawMessage, PruneHistoryStats) {
	return pruneHistory(messages, keepRecent, nil)
}

// pruneHistory is the shared implementation. When store is non-nil, file-read
// results that get stubbed are teed to it (content-addressed) and the stub
// carries a dex:lc_expand:<hash> recovery marker (#597). A nil store is the
// no-CCR path and behaves exactly as before.
func pruneHistory(messages []json.RawMessage, keepRecent int, store *TeeStore) ([]json.RawMessage, PruneHistoryStats) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	if len(messages) <= keepRecent {
		return messages, PruneHistoryStats{}
	}

	// Pass 1: build tool_use_id → tool_name from ALL messages (assistant blocks).
	toolNames := buildToolNameMap(messages)

	out := make([]json.RawMessage, len(messages))
	boundary := pruneStart(len(messages), keepRecent)
	pruneFloor := cachedPrefixLen(messages) // protect client-set cache_control prefix
	var st PruneHistoryStats

	for i, raw := range messages {
		// Keep messages in the keep-recent zone (i >= boundary) or inside the
		// client-set cache_control prefix (i < pruneFloor) byte-identical.
		if i >= boundary || i < pruneFloor {
			out[i] = raw
			continue
		}
		rewritten, ok, preserved := rewriteMessageWithStats(raw, toolNames, &st, store)
		if !ok {
			out[i] = raw // fail-open
			continue
		}
		_ = preserved
		out[i] = rewritten
	}
	return out, st
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

// rewriteMessageWithStats is like rewriteMessage but accumulates stats into st
// (when non-nil). The third return value is the number of blocks preserved.
func rewriteMessageWithStats(raw json.RawMessage, toolNames map[string]string, st *PruneHistoryStats, store *TeeStore) (json.RawMessage, bool, int) {
	// Decode into a generic map to preserve unknown fields.
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil, false, 0
	}
	roleRaw, ok := msg["role"]
	if !ok {
		return nil, false, 0
	}
	var role string
	if json.Unmarshal(roleRaw, &role) != nil {
		return nil, false, 0
	}
	// Only user messages carry tool_result blocks.
	if role != "user" {
		return raw, true, 0
	}

	contentRaw, ok := msg["content"]
	if !ok {
		return raw, true, 0
	}

	// Content can be a bare string (no tool_results possible) or array.
	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return raw, true, 0 // bare string — nothing to prune
	}

	changed := false
	preserved := 0
	newBlocks := make([]json.RawMessage, len(blocks))
	for i, blkRaw := range blocks {
		rewritten, didChange, wasPreserved := rewriteBlock(blkRaw, toolNames, store)
		newBlocks[i] = rewritten
		if didChange {
			changed = true
		}
		if wasPreserved {
			preserved++
			if st != nil {
				st.ResultsPreserved++
				// Rough token estimate: len(text)/4.
				st.TokensPreserved += len(extractToolResultText(rewritten)) / 4
			}
		}
	}
	if !changed {
		return raw, true, preserved
	}

	newContent, err := json.Marshal(newBlocks)
	if err != nil {
		return nil, false, 0
	}
	msg["content"] = newContent
	out, err := json.Marshal(msg)
	if err != nil {
		return nil, false, 0
	}
	return out, true, preserved
}

// rewriteBlock rewrites one content block if it is a tool_result eligible for
// pruning. Returns (rewritten, changed, preserved) where preserved is true
// when the block was actively kept verbatim rather than stubbed.
func rewriteBlock(raw json.RawMessage, toolNames map[string]string, store *TeeStore) (json.RawMessage, bool, bool) {
	var blk map[string]json.RawMessage
	if json.Unmarshal(raw, &blk) != nil {
		return raw, false, false
	}
	typeRaw, ok := blk["type"]
	if !ok {
		return raw, false, false
	}
	var typ string
	if json.Unmarshal(typeRaw, &typ) != nil || typ != "tool_result" {
		return raw, false, false
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
		return raw, false, false
	}
	text := extractToolResultText(contentRaw)

	// Preserve checks run before the length gate so that dex results with
	// saved_pct > 0 and <lc_safe> content are protected even when short.
	if shouldPreserveResult(text) {
		return raw, false, true // kept verbatim — count as preserved
	}

	if len(text) < minPruneChars {
		return raw, false, false // already short; no preserve signal
	}

	// Test/build output: error context is load-bearing; keep verbatim.
	if isTestOutput(text) {
		return raw, false, true
	}

	kind := classifyTool(toolName)
	// Name-based classification whiffs on foreign MCP tools whose names don't
	// carry a read/command keyword (fetch_file, load_document, get_source).
	// Fall back to the content of the result so a source-code body still gets
	// the path-preserving file-read stub instead of a head/tail summary (#615).
	if kind == kindUnknown {
		kind = classifyByContent(text)
	}
	var stub string
	switch kind {
	case kindFileRead:
		lines := countLines(text)
		stub = fmt.Sprintf("[earlier file read (%d lines) pruned to save tokens; re-read if needed]", lines)
		// CCR (#597): tee the original bytes content-addressed and embed a
		// recovery marker so the pruned read can be reconstructed losslessly.
		// No-op when store is nil or the content is below the tee threshold.
		if hash, ok := store.Put(text); ok {
			stub += " " + marker(hash)
		}
	case kindCommand:
		stub = headTailSummary(text)
	default:
		// Unknown tool — use command-style summary (conservative).
		stub = headTailSummary(text)
	}

	// Replace content with the stub string.
	newContent, err := json.Marshal(stub)
	if err != nil {
		return raw, false, false
	}
	blk["content"] = newContent
	out, err := json.Marshal(blk)
	if err != nil {
		return raw, false, false
	}
	return out, true, false
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

// sourceCodeKeywords are declaration tokens that LEAD a line in source across
// the common languages. Matched case-insensitively against the start of a
// trimmed line — declarations begin lines; a prose or commit-message mention of
// "package" or "func" sits mid-line and must not match.
var sourceCodeKeywords = []string{
	"import ", "package ", "func ", "def ", "class ", "const ",
	"public ", "private ", "protected ", "#include", "fn ", "impl ",
	"struct ", "interface ", "function ", "namespace ", "export ",
	"return ", "module ", "var ", "let ",
}

// classifyByContent is the fallback when name-based classification yields
// kindUnknown (#615): a foreign tool whose RESULT looks like source code gets
// the file-read stub (preserves path + line count + CCR tee). Anything else
// stays kindUnknown — which prunes identically to kindCommand (head/tail
// summary) — so command/log/doc output keeps its existing treatment.
func classifyByContent(body string) toolKind {
	if looksLikeSourceCode(body) {
		return kindFileRead
	}
	return kindUnknown
}

// looksLikeSourceCode reports whether s reads like source: a declaration
// keyword at the START of some line AND a density of structural punctuation.
// Line-start matching keeps commit logs, markdown tables, and YAML — which
// merely mention "package"/"func" mid-line — from false-positiving; the
// structural gate keeps a lone prose line starting with "return …" out.
func looksLikeSourceCode(s string) bool {
	head := s
	if len(head) > 4000 {
		head = head[:4000]
	}
	hasDecl := false
	for _, ln := range strings.Split(head, "\n") {
		lower := strings.ToLower(strings.TrimSpace(ln))
		for _, kw := range sourceCodeKeywords {
			if strings.HasPrefix(lower, kw) {
				hasDecl = true
				break
			}
		}
		if hasDecl {
			break
		}
	}
	if !hasDecl {
		return false
	}
	structural := strings.Count(head, "{") + strings.Count(head, "}") +
		strings.Count(head, ";") + strings.Count(head, "(") +
		strings.Count(head, ")") + strings.Count(head, "=")
	return structural >= 3
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
