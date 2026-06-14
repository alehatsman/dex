package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTrigramFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func contains(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestTrigramCacheRebuildsOnFileSetChange locks issue #524: when the candidate
// file SET grows (e.g. a reindex finished and FileTree now returns a file that
// a mid-reindex snapshot omitted), getOrBuild must rebuild rather than serve
// the cached index built from the partial set. Otherwise narrowing drops the
// new file and grep reports a stable false-negative match count.
func TestTrigramCacheRebuildsOnFileSetChange(t *testing.T) {
	dir := t.TempDir()
	a := writeTrigramFile(t, dir, "a.go", "package x\n\nfunc FuseWithSymbols() {}\n")
	b := writeTrigramFile(t, dir, "b.go", "package x\n\nvar unrelated = 1\n")
	c := writeTrigramFile(t, dir, "c.go", "package x\n\nfunc callFuseWithSymbols() {}\n")

	cache := &trigramCache{}
	key := trigramCacheKey{root: dir}

	// Initial build from a PARTIAL set that omits c.go.
	idx1 := cache.getOrBuild(key, []string{a, b})
	cand1, ok := idx1.Narrow("FuseWithSymbols")
	if !ok {
		t.Fatal("Narrow returned ok=false for a word query")
	}
	if contains(cand1, c) {
		t.Fatalf("partial build should not know about c.go yet; got %v", cand1)
	}

	// The file set now includes c.go (reindex completed). Same cache key.
	idx2 := cache.getOrBuild(key, []string{a, b, c})
	cand2, ok := idx2.Narrow("FuseWithSymbols")
	if !ok {
		t.Fatal("Narrow returned ok=false after rebuild")
	}
	if !contains(cand2, c) {
		t.Errorf("grep false-negative (#524): c.go contains the symbol but was not narrowed in; got %v", cand2)
	}
	if !contains(cand2, a) {
		t.Errorf("a.go should still be a candidate; got %v", cand2)
	}
}

func TestFilesFingerprint(t *testing.T) {
	ab := filesFingerprint([]string{"a.go", "b.go"})
	ba := filesFingerprint([]string{"b.go", "a.go"})
	if ab != ba {
		t.Errorf("fingerprint must be order-independent: %d != %d", ab, ba)
	}
	abc := filesFingerprint([]string{"a.go", "b.go", "c.go"})
	if ab == abc {
		t.Error("adding a file must change the fingerprint")
	}
	if filesFingerprint([]string{"a.go"}) == filesFingerprint([]string{"b.go"}) {
		t.Error("different single-file sets must differ")
	}
	if filesFingerprint(nil) != filesFingerprint([]string{}) {
		t.Error("nil and empty should match")
	}
}
