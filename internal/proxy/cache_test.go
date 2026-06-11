package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// bigText returns a string of roughly n tokens (~4 chars/token) of varied
// words so it doesn't collapse under prose dedup and clears the cacheable floor.
func bigText(approxTokens int) string {
	var b strings.Builder
	for i := 0; b.Len() < approxTokens*4; i++ {
		b.WriteString("alpha bravo charlie delta echo foxtrot ")
	}
	return b.String()
}

// countBreakpoints walks tools, system, and every message content block and
// counts cache_control markers in a /v1/messages body.
func countBreakpoints(t *testing.T, body []byte) int {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	n := 0
	countArray := func(r json.RawMessage) {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(r, &arr) != nil {
			return
		}
		for _, o := range arr {
			if _, ok := o["cache_control"]; ok {
				n++
			}
		}
	}
	countArray(raw["tools"])
	countArray(raw["system"])
	var msgs []json.RawMessage
	_ = json.Unmarshal(raw["messages"], &msgs)
	for _, m := range msgs {
		var msg map[string]json.RawMessage
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		countArray(msg["content"])
	}
	return n
}

// TestAlignMarksToolsAndSystem: a large system prompt + tool defs get a
// breakpoint each, capped at maxBreakpoints, none beyond.
func TestAlignMarksToolsAndSystem(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model":  "claude-sonnet-4-5", // 1024-token floor
		"system": bigText(2000),
		"tools": []any{
			map[string]any{"name": "Read", "description": bigText(2000)},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})

	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	if !stats.Applied {
		t.Fatalf("expected breakpoints to be applied; stats=%+v", stats)
	}
	got := countBreakpoints(t, out)
	if got == 0 {
		t.Fatalf("expected at least one breakpoint, got 0")
	}
	if got > maxBreakpoints {
		t.Fatalf("emitted %d breakpoints, exceeds max %d", got, maxBreakpoints)
	}
	if got != stats.Breakpoints {
		t.Errorf("stats.Breakpoints=%d but body has %d markers", stats.Breakpoints, got)
	}
}

// TestAlignNeverExceedsMax: a long conversation with many large stable turns
// must still yield at most maxBreakpoints.
func TestAlignNeverExceedsMax(t *testing.T) {
	var msgs []any
	for i := 0; i < 40; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"role":    role,
			"content": []any{map[string]any{"type": "text", "text": bigText(1500)}},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   bigText(2000),
		"tools":    []any{map[string]any{"name": "Read", "description": bigText(2000)}},
		"messages": msgs,
	})

	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	got := countBreakpoints(t, out)
	if got > maxBreakpoints {
		t.Fatalf("emitted %d breakpoints, exceeds max %d", got, maxBreakpoints)
	}
	if !stats.Applied || got == 0 {
		t.Fatalf("expected breakpoints on a large conversation; stats=%+v got=%d", stats, got)
	}
}

// TestAlignStripsExistingMarkers: Claude Code's own cache_control markers are
// removed and replaced, so the total never compounds past maxBreakpoints.
func TestAlignStripsExistingMarkers(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"system": []any{
			map[string]any{"type": "text", "text": bigText(2000), "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"tools": []any{
			map[string]any{"name": "Read", "description": bigText(2000), "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{
			// A volatile recent message that Claude Code also marked.
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "recent", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
		},
	})

	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	got := countBreakpoints(t, out)
	if got > maxBreakpoints {
		t.Fatalf("emitted %d breakpoints after strip+replace, exceeds max %d", got, maxBreakpoints)
	}
	if !stats.Applied {
		t.Fatalf("expected applied; stats=%+v", stats)
	}
	// The volatile (last keepRecent) message must NOT carry a marker now.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var msgs []json.RawMessage
	_ = json.Unmarshal(raw["messages"], &msgs)
	if strings.Contains(string(msgs[len(msgs)-1]), "cache_control") {
		t.Errorf("volatile tail message should not be marked: %s", msgs[len(msgs)-1])
	}
}

// TestAlignDeterministic: the same input yields byte-identical output — the
// property the cross-turn cache hit depends on.
func TestAlignDeterministic(t *testing.T) {
	build := func() []byte {
		var msgs []any
		for i := 0; i < 20; i++ {
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": bigText(1500)}},
			})
		}
		b, _ := json.Marshal(map[string]any{
			"model":    "claude-sonnet-4-5",
			"system":   bigText(2000),
			"messages": msgs,
		})
		return b
	}
	out1, _ := AlignCacheBreakpoints(build(), DefaultKeepRecent)
	out2, _ := AlignCacheBreakpoints(build(), DefaultKeepRecent)
	if string(out1) != string(out2) {
		t.Errorf("alignment is not deterministic:\n%s\n---\n%s", out1, out2)
	}
}

// TestAlignSkipsSmallPrefix: when nothing clears the model's cacheable floor,
// no breakpoint is placed and the body is returned unchanged.
func TestAlignSkipsSmallPrefix(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-8", // 4096-token floor
		"system":   "short prompt",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	if stats.Applied {
		t.Errorf("did not expect breakpoints for a tiny prefix; stats=%+v", stats)
	}
	if string(out) != string(body) {
		t.Errorf("body should be unchanged when no breakpoint is placed")
	}
	if countBreakpoints(t, out) != 0 {
		t.Errorf("expected 0 breakpoints, got %d", countBreakpoints(t, out))
	}
}

// TestAlignFailOpenMalformed: a non-JSON body is forwarded verbatim.
func TestAlignFailOpenMalformed(t *testing.T) {
	body := []byte(`{not valid json`)
	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	if stats.Applied {
		t.Errorf("malformed body must not be marked")
	}
	if string(out) != string(body) {
		t.Errorf("malformed body must be returned unchanged")
	}
}

// TestAlignVolatileTailUncached: with a short stable region but a large recent
// tail, no breakpoint lands in the volatile window.
func TestAlignVolatileTailUncached(t *testing.T) {
	var msgs []any
	// All messages are within keepRecent (volatile) → stableEnd == 0.
	for i := 0; i < DefaultKeepRecent; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": bigText(2000)}},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"messages": msgs,
	})
	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	if stats.Applied || countBreakpoints(t, out) != 0 {
		t.Errorf("no breakpoint should land in the volatile tail; stats=%+v breakpoints=%d", stats, countBreakpoints(t, out))
	}
	if stats.VolatileTokens == 0 {
		t.Errorf("expected volatile tokens to be counted")
	}
}

// TestAlignMessageBreakpointOnStablePrefix: a long stable conversation gets a
// breakpoint within the pruned region (not just tools/system), and the marker
// sits on the last block of a stable message.
func TestAlignMessageBreakpointOnStablePrefix(t *testing.T) {
	var msgs []any
	for i := 0; i < 30; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": bigText(2000)}},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"messages": msgs,
	})
	out, stats := AlignCacheBreakpoints(body, DefaultKeepRecent)
	if !stats.Applied {
		t.Fatalf("expected breakpoints; stats=%+v", stats)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var outMsgs []json.RawMessage
	_ = json.Unmarshal(raw["messages"], &outMsgs)
	stableEnd := len(outMsgs) - DefaultKeepRecent
	marked := 0
	for i := 0; i < stableEnd; i++ {
		if strings.Contains(string(outMsgs[i]), "cache_control") {
			marked++
		}
	}
	if marked == 0 {
		t.Errorf("expected at least one message breakpoint in the stable region")
	}
	for i := stableEnd; i < len(outMsgs); i++ {
		if strings.Contains(string(outMsgs[i]), "cache_control") {
			t.Errorf("message %d is in the volatile tail and must not be marked", i)
		}
	}
}

func TestMinCacheableTokens(t *testing.T) {
	cases := map[string]int{
		"claude-opus-4-8":            4096,
		"claude-opus-4-6":            4096,
		"claude-haiku-4-5-20251001":  4096,
		"claude-sonnet-4-6":          2048,
		"claude-fable-5":             2048,
		"claude-sonnet-4-5-20250929": 1024,
		"claude-3-7-sonnet-20250219": 1024,
		"some-unknown-model":         4096, // safe-high default
	}
	for model, want := range cases {
		if got := minCacheableTokens(model); got != want {
			t.Errorf("minCacheableTokens(%q)=%d, want %d", model, got, want)
		}
	}
}
