package index

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestConcurrentReindexNeverYieldsZeroForLiveSymbol is the regression guard for
// the read-visibility race root-caused in #524: with many agents working a repo
// at once, every edit triggers the MCP auto-watcher to reconcile + reindex the
// shared store, and a search landing mid-reconcile could observe an empty/stale
// result for a symbol that never actually left the source.
//
// The invariant: while a writer hammers ix.Run (the reconcile loop) on the same
// *store.Store the readers query, NO reader may ever see zero hits for a symbol
// that is present in the source the entire time. A transiently-empty store
// mid-reindex must never be returned as an authoritative empty result.
//
// This is expected to PASS: store.Open uses WAL + a transactional
// upsert-first/prune-last cycle, so readers see a consistent snapshot that
// always contains the live symbol (pre-upsert: old chunks; post-upsert: new ∪
// old; post-prune: new). The test locks that ordering in — a future change that
// deletes-before-reinsert, drops WAL isolation, or rebuilds FTS non-atomically
// would reintroduce #524 and turn this red.
func TestConcurrentReindexNeverYieldsZeroForLiveSymbol(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrency stress test; skipped under -short")
	}

	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	// alpha.go declares Alpha and is never removed — Alpha is "live" for the
	// whole test, so a correct search must always find it.
	const alphaSrc = `package main

// Alpha is the first function. rev %d
func Alpha() string { return "alpha" }
`
	writeFile(t, filepath.Join(projDir, "alpha.go"), fmt.Sprintf(alphaSrc, 0))

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
	ix := New(p, st, em, ig, Options{Verbose: false})

	// Build once so Alpha is present before any reader starts.
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}

	// Query vector for the live symbol; reused by every reader.
	qvecs, err := em.Embed(ctx, []string{"func Alpha() string"})
	if err != nil {
		t.Fatal(err)
	}
	qvec := qvecs[0]

	// Precondition: a steady-state fused search finds Alpha.
	if hits, err := st.SearchFused(ctx, qvec, "Alpha", 5); err != nil || len(hits) == 0 {
		t.Fatalf("precondition: SearchFused must find Alpha in steady state (hits=%d err=%v)", len(hits), err)
	}

	const reindexCycles = 30
	const readers = 8

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		reads    atomic.Int64
		zeroSeen atomic.Int64
	)

	// Writer: the reconcile loop. Each cycle rewrites alpha.go (forcing
	// re-chunk + upsert + prune) and reindexes the shared store.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop.Store(true)
		for i := 1; i <= reindexCycles; i++ {
			writeFile(t, filepath.Join(projDir, "alpha.go"), fmt.Sprintf(alphaSrc, i))
			if err := ix.Run(ctx); err != nil {
				t.Errorf("reindex Run #%d: %v", i, err)
				return
			}
		}
	}()

	// Readers: hammer the fused search path (vector + BM25) for the live symbol.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				hits, err := st.SearchFused(ctx, qvec, "Alpha", 5)
				if err != nil {
					t.Errorf("SearchFused under concurrent reindex: %v", err)
					return
				}
				reads.Add(1)
				if len(hits) == 0 {
					zeroSeen.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	if got := reads.Load(); got == 0 {
		t.Fatal("no reads executed — test did not exercise the race")
	}
	if got := zeroSeen.Load(); got != 0 {
		t.Errorf("read-visibility race (#524): %d of %d concurrent searches saw ZERO hits for the live symbol Alpha",
			got, reads.Load())
	}
	t.Logf("reindex cycles=%d readers=%d reads=%d zero-hit-reads=%d", reindexCycles, readers, reads.Load(), zeroSeen.Load())
}
