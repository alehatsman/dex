package mcp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestOpenStoreDetectsReplacedDB pins #753: PathIndexed must return the
// correct result after the database at dbPath is deleted and recreated (as
// happens during dex reindex). Before the fix, the cached fd pointed at the
// old (now-unlinked) db and PathIndexed returned no_file for every path.
func TestOpenStoreDetectsReplacedDB(t *testing.T) {
	idxDir := t.TempDir()
	projDir := t.TempDir()

	p, err := proj.Resolve(projDir, idxDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}

	// Open once and write a chunk, then close.
	st1, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.UpsertMany(context.Background(), []store.PendingChunk{
		{Path: "main.go", Kind: "fn", Name: "OldFunc", StartLine: 1, EndLine: 3, ContentSHA: "h1", Content: "func OldFunc(){}"},
	}, time.Now()); err != nil {
		_ = st1.Close()
		t.Fatal(err)
	}
	_ = st1.Close()

	s := &Server{IndexDir: idxDir}

	// First call — warms the cache.
	st, err := s.openStore(p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := st.PathIndexed(context.Background(), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("first open: expected main.go to be indexed")
	}

	// Simulate dex reindex: delete the db and create a new one at the same path.
	if err := os.Remove(p.DBPath); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.UpsertMany(context.Background(), []store.PendingChunk{
		{Path: "new.go", Kind: "fn", Name: "NewFunc", StartLine: 1, EndLine: 3, ContentSHA: "h2", Content: "func NewFunc(){}"},
	}, time.Now()); err != nil {
		_ = st2.Close()
		t.Fatal(err)
	}
	_ = st2.Close()

	// Second call must see the NEW database.
	st, err = s.openStore(p.DBPath)
	if err != nil {
		t.Fatal(err)
	}

	// old path must now be gone.
	oldIndexed, err := st.PathIndexed(context.Background(), "main.go")
	if err != nil {
		t.Fatal(err)
	}
	if oldIndexed {
		t.Error("after reindex: main.go should not be indexed in the new db")
	}

	// new path must be present.
	newIndexed, err := st.PathIndexed(context.Background(), "new.go")
	if err != nil {
		t.Fatal(err)
	}
	if !newIndexed {
		t.Error("after reindex: new.go should be indexed in the new db")
	}
}
