package index

import (
	"context"
	"errors"
	"testing"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestRunWithoutEmbedderReturnsErrNoEmbedder proves the lean indexing contract:
// Run with a nil embedder (DEX_EMBED_ENGINE=none) fails fast with ErrNoEmbedder
// instead of nil-panicking on ix.Embed.ModelName(). Indexing needs vectors;
// the lean profile is a serve mode (see #306 for pure no-embedder indexing).
func TestRunWithoutEmbedderReturnsErrNoEmbedder(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, projDir+"/a.go", "package main\n\nfunc A() string { return \"x\" }\n")

	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(context.Background(), p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)

	ix := New(p, st, nil, ig, Options{Verbose: false}) // nil embedder = lean profile

	err := ix.Run(context.Background())
	if !errors.Is(err, ErrNoEmbedder) {
		t.Fatalf("Run with nil embedder: got %v, want ErrNoEmbedder", err)
	}
}
