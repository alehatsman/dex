// Copyright 2026 Aleh Atsman
//
// Regression test for #531: a real index Run must publish the
// indexing-in-progress marker and clear it on completion.

package index

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func TestRunClearsIndexingMarker(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, projDir+"/a.go", "package main\n\nfunc MyFunc() string { return \"x\" }\n")

	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(context.Background(), p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)

	// Pre-seed a marker to prove Run's deferred clear actually fires.
	if err := st.SetIndexing(context.Background(), time.Now()); err != nil {
		t.Fatalf("SetIndexing: %v", err)
	}

	ix := New(p, st, nil, ig, Options{Verbose: false})
	if err := ix.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if in, _ := st.IndexingInProgress(context.Background()); in {
		t.Error("after a completed Run, the indexing marker should be cleared")
	}
}
