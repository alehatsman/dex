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
