package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore builds a TeeStore rooted in a temp dir (already created by
// t.TempDir, so no MkdirAll is needed).
func newTestStore(t *testing.T) *TeeStore {
	t.Helper()
	return &TeeStore{dir: t.TempDir()}
}

func TestTeeStore_PutGetRoundTrip(t *testing.T) {
	ts := newTestStore(t)
	content := strings.Repeat("the quick brown fox\n", 50) // > ccrMinBytes

	hash, ok := ts.Put(content)
	if !ok {
		t.Fatal("Put returned ok=false for above-threshold content")
	}
	if len(hash) != ccrHashLen {
		t.Fatalf("hash len = %d, want %d", len(hash), ccrHashLen)
	}
	got, ok := ts.Get(hash)
	if !ok {
		t.Fatal("Get returned ok=false for stored hash")
	}
	if got != content {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, content)
	}
}

func TestTeeStore_ThresholdSkipsSmall(t *testing.T) {
	ts := newTestStore(t)
	if _, ok := ts.Put(strings.Repeat("x", ccrMinBytes-1)); ok {
		t.Fatal("Put stored content below the threshold")
	}
	// Nothing should have been written.
	entries, _ := os.ReadDir(ts.dir)
	if len(entries) != 0 {
		t.Fatalf("expected empty store, found %d entries", len(entries))
	}
}

func TestTeeStore_RedactsOnWrite(t *testing.T) {
	ts := newTestStore(t)
	secret := "api_key=" + strings.Repeat("A", 40)
	content := secret + "\n" + strings.Repeat("padding line\n", 60)

	hash, ok := ts.Put(content)
	if !ok {
		t.Fatal("Put ok=false")
	}
	got, _ := ts.Get(hash)
	if strings.Contains(got, secret) {
		t.Fatalf("stored bytes still contain the raw secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:secret]") {
		t.Fatalf("stored bytes missing redaction marker: %q", got)
	}
}

func TestTeeStore_ContentAddressedDedup(t *testing.T) {
	ts := newTestStore(t)
	content := strings.Repeat("same bytes\n", 60)
	h1, ok1 := ts.Put(content)
	h2, ok2 := ts.Put(content)
	if !ok1 || !ok2 || h1 != h2 {
		t.Fatalf("same content produced different handles: %q vs %q", h1, h2)
	}
	entries, _ := os.ReadDir(ts.dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 stored file for identical content, found %d", len(entries))
	}
}

func TestTeeStore_GCRemovesExpired(t *testing.T) {
	ts := newTestStore(t)
	hash, ok := ts.Put(strings.Repeat("payload line\n", 60))
	if !ok {
		t.Fatal("Put ok=false")
	}
	path := filepath.Join(ts.dir, hash+".log")

	// Age the entry past the TTL.
	old := time.Now().Add(-ccrTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	ts.MaybeGC()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired entry was not GC'd (stat err=%v)", err)
	}
}

func TestTeeStore_GCThrottled(t *testing.T) {
	ts := newTestStore(t)
	ts.MaybeGC() // first sweep stamps lastGC

	// A fresh entry aged past the TTL would normally be removed, but the
	// throttle should skip the sweep since lastGC was just set.
	hash, _ := ts.Put(strings.Repeat("payload line\n", 60))
	path := filepath.Join(ts.dir, hash+".log")
	old := time.Now().Add(-ccrTTL - time.Hour)
	_ = os.Chtimes(path, old, old)

	ts.MaybeGC() // throttled — should NOT sweep
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("throttled MaybeGC removed entry it should have skipped: %v", err)
	}
}

func TestTeeStore_NilSafe(t *testing.T) {
	var ts *TeeStore
	if _, ok := ts.Put("anything"); ok {
		t.Fatal("nil Put returned ok=true")
	}
	if _, ok := ts.Get("deadbeef"); ok {
		t.Fatal("nil Get returned ok=true")
	}
	ts.MaybeGC() // must not panic
	body := []byte(`{"messages":[]}`)
	out, n := ts.ExpandMarkers(body, DefaultKeepRecent)
	if n != 0 || string(out) != string(body) {
		t.Fatal("nil ExpandMarkers mutated the body")
	}
}

// TestExpandMarkers_RoundTrip proves the inverse primitive: a keep-window
// tool_result carrying a recovery marker is restored to the exact stored bytes.
func TestExpandMarkers_RoundTrip(t *testing.T) {
	ts := newTestStore(t)
	original := strings.Repeat("recovered file content\n", 40)
	hash, ok := ts.Put(original)
	if !ok {
		t.Fatal("Put ok=false")
	}

	// Few messages → boundary 0, so the single user message is in the window.
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "t1",
					"content":     "[earlier file read pruned] " + marker(hash),
				},
			}},
		},
	})

	out, n := ts.ExpandMarkers(body, DefaultKeepRecent)
	if n != 1 {
		t.Fatalf("restored = %d, want 1", n)
	}
	if !strings.Contains(string(out), "recovered file content") {
		t.Fatalf("expanded body missing recovered content: %s", out)
	}
	// The restored content must equal the stored bytes exactly.
	if got := firstToolResultText(t, out); got != original {
		t.Fatalf("restored content mismatch:\n got %q\nwant %q", got, original)
	}
}

func TestExpandMarkers_UnknownHashFailOpen(t *testing.T) {
	ts := newTestStore(t)
	body := mustJSON(t, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1",
					"content": "stub " + marker("0123456789abcdef")},
			}},
		},
	})
	out, n := ts.ExpandMarkers(body, DefaultKeepRecent)
	if n != 0 {
		t.Fatalf("restored = %d, want 0 for unknown hash", n)
	}
	if string(out) != string(body) {
		t.Fatal("body mutated on unknown-hash fail-open")
	}
}

// TestPruneTeeAndExpand_NoNegation is the end-to-end guard: pruning an old-region
// file read tees the bytes and embeds a marker in the OLD region, and the
// keep-window ExpandMarkers pass does NOT expand that old-region marker (which
// would undo pruning).
func TestPruneTeeAndExpand_NoNegation(t *testing.T) {
	ts := newTestStore(t)
	bulky := strings.Repeat("source line of code\n", 40) // > minPruneChars and ccrMinBytes

	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/a.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": bulky},
		}},
	}
	// Pad to 26 messages so the tool_result at index 1 lands in the prune zone:
	// pruneStart(26,10) = (16/16)*16 = 16, so indices 0-15 are pruned.
	for i := 0; i < 24; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "pad"})
	}
	body := mustJSON(t, map[string]any{"model": "claude-sonnet-4-6", "messages": msgs})

	pruned, savedBytes, _ := PruneRequestBody(body, DefaultKeepRecent, ts)
	if savedBytes <= 0 {
		t.Fatal("expected pruning to save bytes")
	}
	// The stub must carry a recovery marker, and the bytes must be retrievable.
	stub := nthToolResultText(t, pruned, 0)
	hash := parseExpandMarker(stub)
	if hash == "" {
		t.Fatalf("pruned stub missing recovery marker: %q", stub)
	}
	if got, ok := ts.Get(hash); !ok || got != bulky {
		t.Fatalf("teed bytes not recoverable by hash (ok=%v)", ok)
	}

	// Expansion scoped to the keep-window must NOT touch the old-region stub.
	expanded, n := ts.ExpandMarkers(pruned, DefaultKeepRecent)
	if n != 0 {
		t.Fatalf("keep-window expansion restored %d old-region markers (should be 0)", n)
	}
	if string(expanded) != string(pruned) {
		t.Fatal("keep-window expansion mutated old-region content (pruning negated)")
	}
}

// TestCollapseReReads_RoundTrip is the Option 2 (#640) identity guard: a
// keep-window file-read whose bytes are already teed collapses to a marker,
// and the companion ExpandMarkers pass restores the exact stored bytes —
// collapse∘expand is identity (modulo redaction, which is a no-op on clean
// content).
func TestCollapseReReads_RoundTrip(t *testing.T) {
	ts := newTestStore(t)
	original := strings.Repeat("re-read file body line\n", 40) // > ccrMinBytes, redaction no-op
	hash, ok := ts.Put(original)
	if !ok {
		t.Fatal("Put ok=false")
	}

	// Single read-result message; few messages → boundary 0 → in-window.
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
					"input": map[string]any{"file_path": "/a.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": original},
			}},
		},
	})

	collapsed, n := ts.CollapseReReads(body, DefaultKeepRecent)
	if n != 1 {
		t.Fatalf("collapsed = %d, want 1", n)
	}
	// The collapsed block must carry the marker and be far smaller.
	stub := nthToolResultText(t, collapsed, 0)
	if parseExpandMarker(stub) != hash {
		t.Fatalf("collapsed block missing the expected marker: %q", stub)
	}
	if len(collapsed) >= len(body) {
		t.Fatalf("collapse did not shrink body: %d >= %d", len(collapsed), len(body))
	}

	// Expand restores the exact stored bytes.
	expanded, restored := ts.ExpandMarkers(collapsed, DefaultKeepRecent)
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	if got := nthToolResultText(t, expanded, 0); got != original {
		t.Fatalf("collapse∘expand not identity:\n got %q\nwant %q", got, original)
	}
}

// TestCollapseReReads_NoHashNoCollapse: a keep-window read whose bytes were
// never teed is left untouched (fail-closed on miss).
func TestCollapseReReads_NoHashNoCollapse(t *testing.T) {
	ts := newTestStore(t)
	body := mustJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "Read",
					"input": map[string]any{"file_path": "/a.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1",
					"content": strings.Repeat("never teed\n", 60)},
			}},
		},
	})
	out, n := ts.CollapseReReads(body, DefaultKeepRecent)
	if n != 0 {
		t.Fatalf("collapsed = %d, want 0 for un-teed content", n)
	}
	if string(out) != string(body) {
		t.Fatal("body mutated when nothing was collapsible")
	}
}

// TestCollapseReReads_CachePrefixUntouched proves the cache-safety invariant:
// collapsing keep-window re-reads leaves every message in the stable prefix
// (index < len-keepRecent) byte-identical, so a cache breakpoint placed there
// is never busted.
func TestCollapseReReads_CachePrefixUntouched(t *testing.T) {
	ts := newTestStore(t)
	body := stubbedReReadBody(t, ts) // tees the old copy, builds old+window reads

	keepRecent := DefaultKeepRecent
	var before struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	boundary := len(before.Messages) - keepRecent

	collapsed, n := ts.CollapseReReads(body, keepRecent)
	if n == 0 {
		t.Fatal("expected at least one keep-window collapse")
	}
	var after struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(collapsed, &after); err != nil {
		t.Fatalf("unmarshal collapsed: %v", err)
	}
	if len(after.Messages) != len(before.Messages) {
		t.Fatalf("message count changed: %d -> %d", len(before.Messages), len(after.Messages))
	}
	for i := 0; i < boundary; i++ {
		if string(after.Messages[i]) != string(before.Messages[i]) {
			t.Fatalf("stable-prefix message %d was mutated by collapse (cache busted)", i)
		}
	}
}

// TestCollapseExpand_ReReadReclaim is the end-to-end Option 2 path mirroring the
// proxy pipeline: an old-region read is pruned+teed, the same file re-read in
// the keep-window is collapsed to a marker (reclaiming the #561 ReReadTokens),
// then ExpandMarkers restores it pre-send — net: a smaller volatile tail, full
// content preserved, old-region pruning never negated.
func TestCollapseExpand_ReReadReclaim(t *testing.T) {
	ts := newTestStore(t)
	body := stubbedReReadBody(t, ts)

	// Prune first (as proxy.go does): tees + stubs the old-region copy.
	pruned, saved, _ := PruneRequestBody(body, DefaultKeepRecent, ts)
	if saved <= 0 {
		t.Fatal("expected pruning to save bytes")
	}

	// Collapse the keep-window re-read of the now-teed file.
	collapsed, nCollapse := ts.CollapseReReads(pruned, DefaultKeepRecent)
	if nCollapse == 0 {
		t.Fatal("expected the keep-window re-read to collapse")
	}
	if len(collapsed) >= len(pruned) {
		t.Fatalf("collapse did not reclaim bytes: %d >= %d", len(collapsed), len(pruned))
	}

	// Expand restores exact bytes; counts must agree (collapse∘expand identity).
	expanded, restored := ts.ExpandMarkers(collapsed, DefaultKeepRecent)
	if restored != nCollapse {
		t.Fatalf("restored %d != collapsed %d", restored, nCollapse)
	}
	// The keep-window read content is whole again after expansion.
	if !strings.Contains(string(expanded), "source line of code") {
		t.Fatal("expanded body lost the re-read content")
	}
}

// stubbedReReadBody builds a /v1/messages body with a file read in the OLD
// region and a re-read of the SAME content in the keep-window, padded so the
// old copy lands in the prune zone. It pre-tees the content into ts so a
// collapse pass will hash-hit. Returns the marshaled body.
func stubbedReReadBody(t *testing.T, ts *TeeStore) []byte {
	t.Helper()
	bulky := strings.Repeat("source line of code\n", 40) // > minPruneChars and ccrMinBytes
	if _, ok := ts.Put(bulky); !ok {
		t.Fatal("pre-tee Put ok=false")
	}

	read := func(id string) []any {
		return []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "Read",
					"input": map[string]any{"file_path": "/a.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": bulky},
			}},
		}
	}

	var msgs []any
	msgs = append(msgs, read("old")...) // indices 0,1 — old region
	// Pad to 26 messages so pruneStart(26,10)=16: the old read (idx 1) lands in
	// the prune zone, and the re-read (idx 25) in the keep-window (>=16).
	for i := 0; i < 22; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "pad"})
	}
	msgs = append(msgs, read("win")...) // indices 24,25 — keep-window re-read

	return mustJSON(t, map[string]any{"model": "claude-sonnet-4-6", "messages": msgs})
}

// --- helpers ---

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// firstToolResultText returns the flattened text of the first tool_result block.
func firstToolResultText(t *testing.T, body []byte) string {
	t.Helper()
	return nthToolResultText(t, body, 0)
}

// nthToolResultText returns the flattened text of the n-th tool_result block
// (in message/block order) found in body.
func nthToolResultText(t *testing.T, body []byte, n int) string {
	t.Helper()
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	idx := 0
	for _, raw := range req.Messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, blk := range blocks {
			var typ string
			if t2, ok := blk["type"]; !ok || json.Unmarshal(t2, &typ) != nil || typ != "tool_result" {
				continue
			}
			if idx == n {
				return extractToolResultText(blk["content"])
			}
			idx++
		}
	}
	t.Fatalf("no tool_result block at index %d", n)
	return ""
}
