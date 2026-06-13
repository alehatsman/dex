package proxy

import (
	"encoding/json"
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// Port of lean-ctx rust/src/core/neural/cache_alignment.rs, adapted to the
// Anthropic /v1/messages wire format.
//
// dex has a stable-first attention LAYOUT (server_attention.go) but emits no
// explicit Anthropic cache_control breakpoints — until the proxy (#232) put it
// in the request path. AlignCacheBreakpoints marks the stable prefix of a
// request — tool defs, system prompt, CLAUDE.md, early turns — with
// cache_control:{type:"ephemeral"} so Anthropic's KV-cache is reused across
// turns. The volatile tail (the most-recent messages, which change every turn)
// is left uncached.
//
// ORDERING (#237 interplay): this pass MUST run AFTER PruneRequestBody.
// Pruning rewrites old tool_results in the stable region;
// if breakpoints were placed before pruning they would sit on bytes that prune
// then changes, busting the very cache they mark. Placed after pruning, the
// marked prefix is the deterministic post-pruned region: turn N caches it, turn
// N+1 sends a byte-identical (longer) prefix and reads the cache. See proxy.go.

// maxBreakpoints is Anthropic's hard ceiling on cache_control markers per
// request. A request with more is rejected (400), so we never emit more — and
// because Claude Code already sets its own breakpoints, we strip those first
// (stripCacheControl) and re-place our own within this budget.
const maxBreakpoints = 4

// minCacheableTokens returns the minimum cacheable-prefix size for a model, in
// tokens. A cache_control breakpoint whose cumulative prefix is below this is
// silently ignored by Anthropic (cache_creation_input_tokens: 0, no error) — a
// wasted breakpoint slot. The floor is model-dependent (per the prompt-caching
// docs), and the issue's "~1024" only holds for Sonnet 4.5 and older:
//
//	4096 — Opus 4.x, Haiku 4.5      (what Claude Code runs by default)
//	2048 — Fable 5, Sonnet 4.6, Haiku 3.x
//	1024 — Sonnet 4.5 / 4.1 / 4 / 3.7
//
// Unknown models default to the safe-high 4096: overestimating the floor only
// makes us place fewer/deeper breakpoints (always still cacheable);
// underestimating would place breakpoints that never cache.
func minCacheableTokens(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "sonnet-4-5"),
		strings.Contains(m, "sonnet-4-1"),
		strings.Contains(m, "sonnet-4-0"),
		strings.Contains(m, "sonnet-4-2025"),
		strings.Contains(m, "3-7-sonnet"):
		return 1024
	case strings.Contains(m, "fable"),
		strings.Contains(m, "mythos"),
		strings.Contains(m, "sonnet-4-6"),
		strings.Contains(m, "haiku-3"),
		strings.Contains(m, "3-5-haiku"),
		strings.Contains(m, "3-haiku"):
		return 2048
	default: // opus-4.x, haiku-4-5, and anything unrecognized → safe-high floor
		return 4096
	}
}

// CacheStats is the per-request outcome of AlignCacheBreakpoints, recorded into
// Stats and logged alongside the #239 metrics. Applied is false on fail-open.
type CacheStats struct {
	Applied        bool
	Breakpoints    int
	StableTokens   int
	VolatileTokens int
}

// Efficiency is stable_tokens / (stable + volatile) — the share of input the
// breakpoints make cacheable across turns. 0 when there is no input.
func (c CacheStats) Efficiency() float64 {
	total := c.StableTokens + c.VolatileTokens
	if total == 0 {
		return 0
	}
	return float64(c.StableTokens) / float64(total)
}

// cutKind tags where a candidate breakpoint attaches, since cache_control binds
// differently to a tool def vs a system block vs a message content block.
type cutKind int

const (
	cutTools   cutKind = iota // last tool definition
	cutSystem                 // last system content block
	cutMessage                // last content block of messages[idx]
)

// cutPoint is a candidate breakpoint at a structural seam, carrying the
// cumulative stable-token count up to and including it (in render order:
// tools → system → messages). cumTokens drives the ≥minCacheableTokens spacing.
type cutPoint struct {
	kind      cutKind
	msgIdx    int
	cumTokens int
}

// AlignCacheBreakpoints marks the stable prefix of a /v1/messages body with up
// to maxBreakpoints cache_control:{type:"ephemeral"} breakpoints and returns
// the rewritten body plus a CacheStats. The volatile tail (the last keepRecent
// messages) is never marked. Fail-open: on any parse/shape error the original
// body is returned unchanged with a zero (Applied:false) CacheStats.
//
// keepRecent should match the pruning window (DefaultKeepRecent) so the
// "volatile" region lines up exactly with the un-pruned recent messages — the
// stable region is then precisely the deterministically-pruned prefix.
func AlignCacheBreakpoints(body []byte, keepRecent int) ([]byte, CacheStats) {
	if keepRecent < 0 {
		keepRecent = 0
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, CacheStats{}
	}

	// Model drives both the token-counting family and the cacheable floor.
	var model string
	if mRaw, ok := raw["model"]; ok {
		_ = json.Unmarshal(mRaw, &model)
	}
	fam := tokens.Detect(model)
	minTokens := minCacheableTokens(model)

	var messages []json.RawMessage
	if msgsRaw, ok := raw["messages"]; ok {
		if err := json.Unmarshal(msgsRaw, &messages); err != nil {
			return body, CacheStats{} // unexpected shape — forward unmodified
		}
	}

	// Stable region = everything before the trailing keepRecent messages.
	stableEnd := len(messages) - keepRecent
	if stableEnd < 0 {
		stableEnd = 0
	}

	// Build candidate cut points with cumulative token counts in render order.
	candidates, stableTokens := buildCutPoints(raw, messages, stableEnd, fam)

	// Volatile tail token count (for the efficiency metric only).
	volatileTokens := 0
	for i := stableEnd; i < len(messages); i++ {
		volatileTokens += messageTokens(messages[i], fam)
	}

	selected := selectBreakpoints(candidates, minTokens)
	if len(selected) == 0 {
		// Nothing worth caching — leave the request (and any existing markers)
		// untouched rather than stripping for no gain.
		return body, CacheStats{StableTokens: stableTokens, VolatileTokens: volatileTokens}
	}

	// We are placing our own breakpoints: strip Claude Code's existing ones
	// first so the total stays within maxBreakpoints and our deterministic
	// placement is the only one in effect.
	stripCacheControl(raw, messages)

	if err := applyBreakpoints(raw, messages, selected); err != nil {
		return body, CacheStats{} // fail-open on any rewrite error
	}

	newMsgs, err := json.Marshal(messages)
	if err != nil {
		return body, CacheStats{}
	}
	raw["messages"] = newMsgs

	out, err := json.Marshal(raw)
	if err != nil {
		return body, CacheStats{}
	}

	return out, CacheStats{
		Applied:        true,
		Breakpoints:    len(selected),
		StableTokens:   stableTokens,
		VolatileTokens: volatileTokens,
	}
}

// buildCutPoints walks the request in render order (tools → system → stable
// messages) accumulating tokens, and returns one cutPoint per structural seam
// plus the total stable-token count. Empty tools/system are skipped.
func buildCutPoints(raw map[string]json.RawMessage, messages []json.RawMessage, stableEnd int, fam tokens.Family) ([]cutPoint, int) {
	var candidates []cutPoint
	cum := 0

	if toolsRaw, ok := raw["tools"]; ok && hasArrayElements(toolsRaw) {
		cum += tokens.CountFor(string(toolsRaw), fam)
		candidates = append(candidates, cutPoint{kind: cutTools, cumTokens: cum})
	}

	if sysRaw, ok := raw["system"]; ok && len(sysRaw) > 0 {
		var b strings.Builder
		extractText(&b, sysRaw)
		cum += tokens.CountFor(b.String(), fam)
		candidates = append(candidates, cutPoint{kind: cutSystem, cumTokens: cum})
	}

	for i := 0; i < stableEnd; i++ {
		cum += messageTokens(messages[i], fam)
		candidates = append(candidates, cutPoint{kind: cutMessage, msgIdx: i, cumTokens: cum})
	}

	return candidates, cum
}

// selectBreakpoints picks up to maxBreakpoints cut points, walking from the
// deepest (most stable coverage) backward and keeping a point only when at
// least minTokens of fresh content separates it from the last kept point — so
// every cached segment is ≥ the model's cacheable floor. The result is returned
// in render order. The deepest point must itself clear minTokens (its full
// prefix must be cacheable) to be selected at all.
func selectBreakpoints(candidates []cutPoint, minTokens int) []cutPoint {
	var selected []cutPoint
	lastCum := 0
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if len(selected) == 0 {
			if c.cumTokens < minTokens {
				continue // deepest cacheable prefix not reached yet
			}
		} else if lastCum-c.cumTokens < minTokens {
			continue // too close to the last breakpoint to add a cacheable segment
		}
		selected = append(selected, c)
		lastCum = c.cumTokens
		if len(selected) == maxBreakpoints {
			break
		}
	}
	// selected is deepest-first; reverse to render order.
	for l, r := 0, len(selected)-1; l < r; l, r = l+1, r-1 {
		selected[l], selected[r] = selected[r], selected[l]
	}
	return selected
}

// applyBreakpoints attaches cache_control:{type:"ephemeral"} at each selected
// cut point, rewriting raw["tools"], raw["system"], and messages in place.
func applyBreakpoints(raw map[string]json.RawMessage, messages []json.RawMessage, selected []cutPoint) error {
	for _, c := range selected {
		switch c.kind {
		case cutTools:
			marked, err := markLastArrayElement(raw["tools"])
			if err != nil {
				return err
			}
			raw["tools"] = marked
		case cutSystem:
			marked, err := markSystem(raw["system"])
			if err != nil {
				return err
			}
			raw["system"] = marked
		case cutMessage:
			marked, err := markMessage(messages[c.msgIdx])
			if err != nil {
				return err
			}
			messages[c.msgIdx] = marked
		}
	}
	return nil
}

// ephemeral is the cache_control value attached at every breakpoint.
var ephemeral = json.RawMessage(`{"type":"ephemeral"}`)

// markLastArrayElement adds cache_control to the last element of a JSON array
// of objects (used for the tools array). Tools with defer_loading:true are
// skipped — the Anthropic API rejects the combination of both flags.
func markLastArrayElement(arrRaw json.RawMessage) (json.RawMessage, error) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(arrRaw, &arr); err != nil {
		return nil, err
	}
	idx := -1
	for i := len(arr) - 1; i >= 0; i-- {
		if raw, ok := arr[i]["defer_loading"]; !ok || string(raw) != "true" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return arrRaw, nil
	}
	arr[idx]["cache_control"] = ephemeral
	return json.Marshal(arr)
}

// markSystem adds cache_control to the system field. A bare-string system is
// wrapped into a single text block (cache_control can't attach to a raw
// string); an array gets the marker on its last block.
func markSystem(sysRaw json.RawMessage) (json.RawMessage, error) {
	var s string
	if json.Unmarshal(sysRaw, &s) == nil {
		block := map[string]json.RawMessage{
			"type":          json.RawMessage(`"text"`),
			"cache_control": ephemeral,
		}
		textJSON, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		block["text"] = textJSON
		return json.Marshal([]map[string]json.RawMessage{block})
	}
	return markLastArrayElement(sysRaw)
}

// markMessage adds cache_control to the last content block of a message. A
// message whose content is a bare string is rewritten into a single text block.
func markMessage(msgRaw json.RawMessage) (json.RawMessage, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return nil, err
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return msgRaw, nil
	}

	var s string
	if json.Unmarshal(contentRaw, &s) == nil {
		block := map[string]json.RawMessage{
			"type":          json.RawMessage(`"text"`),
			"cache_control": ephemeral,
		}
		textJSON, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		block["text"] = textJSON
		newContent, err := json.Marshal([]map[string]json.RawMessage{block})
		if err != nil {
			return nil, err
		}
		msg["content"] = newContent
		return json.Marshal(msg)
	}

	marked, err := markLastArrayElement(contentRaw)
	if err != nil {
		return nil, err
	}
	msg["content"] = marked
	return json.Marshal(msg)
}

// stripCacheControl removes any existing cache_control markers from tools,
// system, and all message content blocks — so the proxy's deterministic
// placement is the only one in effect and the request stays within
// maxBreakpoints. Best-effort: malformed sub-trees are left as-is.
func stripCacheControl(raw map[string]json.RawMessage, messages []json.RawMessage) {
	if t, ok := raw["tools"]; ok {
		if cleaned, changed := stripFromArray(t); changed {
			raw["tools"] = cleaned
		}
	}
	if s, ok := raw["system"]; ok {
		if cleaned, changed := stripFromArray(s); changed {
			raw["system"] = cleaned
		}
	}
	for i, m := range messages {
		if cleaned, changed := stripFromMessage(m); changed {
			messages[i] = cleaned
		}
	}
}

// stripFromArray removes cache_control from each object in a JSON array.
// Returns (cleaned, changed); a non-array or parse failure yields (raw, false).
func stripFromArray(arrRaw json.RawMessage) (json.RawMessage, bool) {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(arrRaw, &arr) != nil {
		return arrRaw, false
	}
	changed := false
	for _, obj := range arr {
		if _, ok := obj["cache_control"]; ok {
			delete(obj, "cache_control")
			changed = true
		}
	}
	if !changed {
		return arrRaw, false
	}
	out, err := json.Marshal(arr)
	if err != nil {
		return arrRaw, false
	}
	return out, true
}

// stripFromMessage removes cache_control from a message's content blocks (when
// content is an array). A bare-string content carries no markers.
func stripFromMessage(msgRaw json.RawMessage) (json.RawMessage, bool) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(msgRaw, &msg) != nil {
		return msgRaw, false
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return msgRaw, false
	}
	cleaned, changed := stripFromArray(contentRaw)
	if !changed {
		return msgRaw, false
	}
	msg["content"] = cleaned
	out, err := json.Marshal(msg)
	if err != nil {
		return msgRaw, false
	}
	return out, true
}

// messageTokens estimates the token count of one message's content via the
// shared extractText flattener (same accounting as countBodyTokens).
func messageTokens(msgRaw json.RawMessage, fam tokens.Family) int {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return 0
	}
	var b strings.Builder
	extractText(&b, msg.Content)
	return tokens.CountFor(b.String(), fam)
}

// hasArrayElements reports whether raw is a JSON array with at least one element.
func hasArrayElements(raw json.RawMessage) bool {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return false
	}
	return len(arr) > 0
}
