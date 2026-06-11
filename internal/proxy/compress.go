package proxy

import (
	"encoding/json"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/tokens"
)

// CompressRequestBody applies the in-flight tool_result compression funnel to
// all messages in a /v1/messages request body. It is complementary to
// PruneRequestBody — pruning handles OLD results (outside keep_recent), this
// squeezes results across all messages including recent ones.
//
// Returns the (possibly rewritten) body and the number of bytes saved. If
// nothing changes the original body is returned unchanged.
func CompressRequestBody(body []byte) ([]byte, int) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, 0
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, 0
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return body, 0
	}

	toolNames := buildToolNameMap(messages) // reuse from prune.go

	compressed, changed := compressMessages(messages, toolNames)
	if !changed {
		return body, 0
	}
	newMsgs, err := json.Marshal(compressed)
	if err != nil {
		return body, 0
	}
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

// compressMessages applies compressMessage to every message, returning the
// rewritten slice and whether anything changed.
func compressMessages(messages []json.RawMessage, toolNames map[string]string) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, len(messages))
	changed := false
	for i, raw := range messages {
		rewritten, ok := compressMessage(raw, toolNames)
		if !ok {
			out[i] = raw
			continue
		}
		out[i] = rewritten
		if string(rewritten) != string(raw) {
			changed = true
		}
	}
	return out, changed
}

// compressMessage rewrites tool_result blocks in one message by running each
// through the appropriate compression funnel. Fail-open on any error.
func compressMessage(raw json.RawMessage, toolNames map[string]string) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw, false
	}
	roleRaw, ok := msg["role"]
	if !ok {
		return raw, false
	}
	var role string
	if json.Unmarshal(roleRaw, &role) != nil {
		return raw, false
	}
	if role != "user" {
		return raw, true
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return raw, true
	}
	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return raw, true // bare string — nothing to compress
	}

	anyChanged := false
	newBlocks := make([]json.RawMessage, len(blocks))
	for i, blkRaw := range blocks {
		rewritten, didChange := compressBlock(blkRaw, toolNames)
		newBlocks[i] = rewritten
		if didChange {
			anyChanged = true
		}
	}
	if !anyChanged {
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

// compressBlock applies the compression funnel to one content block.
// Returns (rewritten, changed).
func compressBlock(raw json.RawMessage, toolNames map[string]string) (json.RawMessage, bool) {
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

	var toolUseID string
	if idRaw, ok := blk["tool_use_id"]; ok {
		_ = json.Unmarshal(idRaw, &toolUseID)
	}
	toolName := toolNames[toolUseID]

	contentRaw, hasContent := blk["content"]
	if !hasContent {
		return raw, false
	}
	text := extractToolResultText(contentRaw) // reuse from prune.go
	if len(text) < minPruneChars {
		return raw, false
	}

	squeezed := compressToolResult(text, toolName)
	if squeezed == text {
		return raw, false
	}

	newContent, err := json.Marshal(squeezed)
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

// compressToolResult routes the tool_result content through the appropriate
// compressor and returns the result. Returns the original if no savings.
//
// Routing funnel (port of lean-ctx compress.rs:19-126):
//  1. Skip if content < minPruneChars (already cheap).
//  2. Prose (letter-dense, long lines, few code symbols) → prose squeeze
//     (dedup + blank-collapse). Only replace when tokens drop.
//  3. Everything else (shell/build/search/code) → EntropyFilter +
//     TerseCompress. Only replace when tokens drop.
func compressToolResult(content, toolName string) string {
	if len(content) < minPruneChars {
		return content
	}
	if isProse(content) {
		return squeezeProse(content)
	}
	return compressCode(content, toolName)
}

// minTokenSaving is the minimum fractional reduction required to emit the
// compressed output. Below this the original is returned unchanged.
const minTokenSaving = 0.03

// isProse reports whether text looks like natural-language prose rather than
// code or log output. Heuristics mirror lean-ctx tool_kind.rs:
//   - average line length > 60 (prose flows in long lines)
//   - symbol density < 8% (few {, }, (, ), [, ], ;, <, >)
//   - letter density > 65% (mostly alphabetic)
func isProse(text string) bool {
	if text == "" {
		return false
	}
	lines := strings.Split(text, "\n")
	var totalLen, symbolCount, letterCount, totalChars int
	for _, line := range lines {
		totalLen += len(line)
		for _, r := range line {
			totalChars++
			switch r {
			case '{', '}', '(', ')', '[', ']', ';', '<', '>':
				symbolCount++
			default:
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					letterCount++
				}
			}
		}
	}
	if totalChars == 0 {
		return false
	}
	avgLineLen := float64(totalLen) / float64(len(lines))
	symbolDensity := float64(symbolCount) / float64(totalChars)
	letterDensity := float64(letterCount) / float64(totalChars)
	return avgLineLen > 60 && symbolDensity < 0.08 && letterDensity > 0.65
}

// squeezeProse applies dedup and blank-collapse to prose content.
// Returns the original if no tokens are saved.
func squeezeProse(content string) string {
	lines := strings.Split(content, "\n")
	seen := make(map[string]struct{}, len(lines))
	var out []string
	prevBlank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !prevBlank {
				out = append(out, "")
			}
			prevBlank = true
			continue
		}
		prevBlank = false
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, line)
	}
	result := strings.Join(out, "\n")
	fam := tokens.Cl100k
	origTok := tokens.CountFor(content, fam)
	newTok := tokens.CountFor(result, fam)
	if newTok >= origTok || float64(origTok-newTok)/float64(origTok) < minTokenSaving {
		return content
	}
	return result
}

// compressCode applies EntropyFilter + TerseCompress to shell/build/search/code
// output. Returns the original if no tokens are saved.
func compressCode(content, toolName string) string {
	lines := strings.Split(content, "\n")
	filtered := compress.EntropyFilter(lines, compress.EntropyThresholdStandard)
	if filtered == nil {
		filtered = lines
	}
	joined := strings.Join(filtered, "\n")
	tr := compress.TerseCompress(joined, compress.Level3)
	if !tr.Applied {
		return content
	}
	fam := tokens.Cl100k
	origTok := tokens.CountFor(content, fam)
	newTok := tokens.CountFor(tr.Output, fam)
	if newTok >= origTok || float64(origTok-newTok)/float64(origTok) < minTokenSaving {
		return content
	}
	return tr.Output
}
