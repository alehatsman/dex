package index

import (
	"context"
	"testing"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestRunWithoutEmbedderBuildsBM25Index proves that Run() with a nil embedder
// (DEX_EMBED_ENGINE=none) succeeds and produces a queryable BM25/symbol/graph
// index — no vectors, no panic, no ErrNoEmbedder. Issue #306.
func TestRunWithoutEmbedderBuildsBM25Index(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, projDir+"/a.go", "package main\n\nfunc MyFunc() string { return \"lean\" }\n")

	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(context.Background(), p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)

	ix := New(p, st, nil, ig, Options{Verbose: false}) // nil embedder = lean / BM25-only

	if err := ix.Run(context.Background()); err != nil {
		t.Fatalf("Run with nil embedder: unexpected error %v", err)
	}

	// search_symbol must work — uses the symbols table, not vec0.
	hits, err := st.FindSymbol(context.Background(), "MyFunc", 10)
	if err != nil {
		t.Fatalf("FindSymbol after lean index: %v", err)
	}
	if len(hits) == 0 {
		t.Error("FindSymbol returned no hits — expected function MyFunc")
	}

	// BM25 search must work with multi-char token.
	bm25, err := st.Search(context.Background(), nil, "lean", 10)
	if err != nil {
		t.Fatalf("Search (BM25-only) after lean index: %v", err)
	}
	if len(bm25) == 0 {
		t.Error("BM25 Search returned no hits — expected chunk containing 'lean'")
	}
}
