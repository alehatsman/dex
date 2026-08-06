package veccache

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func openTemp(t *testing.T, maxRows int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), FileName), maxRows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := openTemp(t, 0)
	ctx := context.Background()
	entries := map[string][]float32{
		"k1": {1, 2, 3, 4},
		"k2": {-0.5, 0.25, 0, 9},
	}
	if err := s.Put(ctx, entries); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, []string{"k1", "k2", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (missing key must be absent)", len(got))
	}
	for k, want := range entries {
		if !equalVec(got[k], want) {
			t.Errorf("key %s: got %v, want %v", k, got[k], want)
		}
	}
	if _, ok := got["missing"]; ok {
		t.Error("absent key must not appear in the result map")
	}
}

func TestGetEmptyKeys(t *testing.T) {
	s := openTemp(t, 0)
	got, err := s.Get(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty query returned %d entries, want 0", len(got))
	}
}

// TestPutIsIdempotent: re-Putting a key keeps the first vector (INSERT OR
// IGNORE) — a key's vector is a pure function of the key.
func TestPutIsIdempotent(t *testing.T) {
	s := openTemp(t, 0)
	ctx := context.Background()
	if err := s.Put(ctx, map[string][]float32{"k": {1, 1, 1, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, map[string][]float32{"k": {9, 9, 9, 9}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, []string{"k"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalVec(got["k"], []float32{1, 1, 1, 1}) {
		t.Errorf("re-Put overwrote the vector: got %v, want the original", got["k"])
	}
}

// TestPruneBoundsRowCount: prune runs on Open and caps the table to maxRows.
func TestPruneBoundsRowCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, 0) // unbounded
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		// Separate Put calls so created_at advances, giving prune a stable
		// oldest-first order to evict.
		if err := s.Put(ctx, map[string][]float32{fmt.Sprintf("k%02d", i): {float32(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()

	// Reopen with a cap → prune fires on Open.
	s2, err := Open(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("row count after prune = %d, want 4", n)
	}
}

// TestPeriodicPruneBoundsLongLivedStore: a Store that is never reopened (the
// MCP auto-watcher case) still bounds its row count via periodic prune.
func TestPeriodicPruneBoundsLongLivedStore(t *testing.T) {
	s := openTemp(t, 3)
	s.pruneEvery = 4 // white-box: prune every 4 inserted rows
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		if err := s.Put(ctx, map[string][]float32{fmt.Sprintf("k%02d", i): {float32(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n > s.maxRows+s.pruneEvery {
		t.Fatalf("row count %d exceeds bound maxRows(%d)+pruneEvery(%d) — periodic prune not firing",
			n, s.maxRows, s.pruneEvery)
	}
	if n >= 20 {
		t.Fatalf("row count %d — nothing pruned across 20 inserts without a reopen", n)
	}
}

func TestMaxRowsFromEnv(t *testing.T) {
	t.Setenv("DEX_VEC_CACHE_MAX", "")
	if got := MaxRowsFromEnv(); got != DefaultMaxRows {
		t.Errorf("unset → %d, want default %d", got, DefaultMaxRows)
	}
	t.Setenv("DEX_VEC_CACHE_MAX", "42")
	if got := MaxRowsFromEnv(); got != 42 {
		t.Errorf("=42 → %d, want 42", got)
	}
	t.Setenv("DEX_VEC_CACHE_MAX", "0")
	if got := MaxRowsFromEnv(); got != 0 {
		t.Errorf("=0 → %d, want 0 (unbounded)", got)
	}
	t.Setenv("DEX_VEC_CACHE_MAX", "garbage")
	if got := MaxRowsFromEnv(); got != DefaultMaxRows {
		t.Errorf("garbage → %d, want default %d", got, DefaultMaxRows)
	}
}

// TestOpenOnDirFails: a non-openable path surfaces an error at Open so the
// caller can degrade to no-cache (WithCache(em, nil)).
func TestOpenOnDirFails(t *testing.T) {
	if _, err := Open(t.TempDir(), 0); err == nil {
		t.Fatal("opening a directory as the cache DB should error")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	v := []float32{1.5, -2.25, 0, 3.75}
	if !equalVec(decodeVec(encodeVec(v)), v) {
		t.Error("encode∘decode is not the identity")
	}
}

func TestDecodeVecRejectsBadLength(t *testing.T) {
	if decodeVec([]byte{1, 2, 3}) != nil {
		t.Error("a non-multiple-of-4 blob must decode to nil (treated as a miss)")
	}
	if decodeVec(nil) != nil {
		t.Error("an empty blob must decode to nil")
	}
}
