package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestInterruptedRunForcesFullReindex pins #806: when a previous index run is
// interrupted partway through (e.g. an exec-reload via SIGUSR1 kills the
// process), it leaves the "indexing in progress" marker set and some files
// with an incomplete chunk set. The next Run must detect the leftover marker
// and force a full reindex — bypassing the mtime fast-path that would
// otherwise just re-stamp the partial chunks and leave the file broken.
func TestInterruptedRunForcesFullReindex(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	const newSrc = `package main

// Alpha is the first function.
func Alpha() string { return "alpha" }

// Beta is the second function.
func Beta() string { return "beta" }

// Gamma is the third function.
func Gamma() string { return "gamma" }
`
	aPath := filepath.Join(projDir, "alpha.go")
	writeFile(t, aPath, newSrc)

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 8, 10*time.Second)
	ix := New(p, st, em, ig, Options{})

	if err := ix.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}

	// Sanity: alpha.go is fully chunked and Gamma is present.
	full, err := st.ChunkBodiesByPath(ctx, "alpha.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(full) == 0 || !containsBody(full, "Gamma") {
		t.Fatalf("precondition: expected fully-chunked alpha.go incl. Gamma, got %d chunks", len(full))
	}

	// Simulate the interrupted-run damage: drop all but the FIRST chunk of
	// alpha.go, leaving a partial chunk set (mirrors #806's "chunks only for a
	// sub-range" symptom). Re-stamp the survivor so TouchPath finds rows>0.
	if err := st.DeletePath(ctx, "alpha.go"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMany(ctx, []store.PendingChunk{{
		Path: "alpha.go", Kind: "function", StartLine: full[0].StartLine, EndLine: full[0].EndLine,
		ContentSHA: full[0].ContentSHA1, Content: full[0].Content, Vec: hashVec(full[0].Content, 16),
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Ensure alpha.go's mtime is in the past so it WOULD hit the mtime
	// fast-path (and stay broken) on a normal run.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(aPath, past, past); err != nil {
		t.Fatal(err)
	}

	// Mark an interrupted run: lastIndexedAt is recent (set by the first Run),
	// the marker lingers because the run never cleared it.
	if err := st.SetIndexing(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	partial, err := st.ChunkBodiesByPath(ctx, "alpha.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 1 {
		t.Fatalf("setup: expected 1 partial chunk, got %d", len(partial))
	}

	// Second Run: the lingering marker must force a full reindex, repairing
	// alpha.go back to its complete chunk set despite the past mtime.
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("repair Run: %v", err)
	}

	repaired, err := st.ChunkBodiesByPath(ctx, "alpha.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) < len(full) {
		t.Errorf("alpha.go not fully repaired: got %d chunks, want >= %d", len(repaired), len(full))
	}
	if !containsBody(repaired, "Gamma") {
		t.Errorf("alpha.go repair lost Gamma chunk: %d chunks present", len(repaired))
	}
}

func containsBody(bodies []store.ChunkBody, want string) bool {
	for _, b := range bodies {
		if strings.Contains(b.Content, want) {
			return true
		}
	}
	return false
}
