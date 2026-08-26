package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestStoreSingleConnectionPinned locks the #185 fix: the store pool is capped
// at one connection. That cap is what makes SQLITE_BUSY_SNAPSHOT structurally
// impossible — a second pool connection is what would upgrade a stale read
// snapshot to a write and deadlock non-retriably.
func TestStoreSingleConnectionPinned(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/index.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (store must be pinned to a single writer, #185)", got)
	}
}

// chunkCount returns the number of rows in the chunks table — a raw read leg
// for the contention test that doesn't depend on any higher-level query path.
func chunkCount(ctx context.Context, t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

// TestStoreConcurrentReadWriteContention hammers one *Store with concurrent
// transactional writes (UpsertMany → BeginTx) and reads (a COUNT over chunks).
// Two things are under test:
//
//   - No SQLITE_BUSY* surfaces — the single connection serializes access so the
//     snapshot-upgrade race can't happen (#185).
//   - No deadlock — every write transaction is self-contained (uses only its tx
//     handle, never re-enters the pool), so pinning the pool to one connection
//     does not starve a tx of the connection it already holds. A regression that
//     re-entered the pool inside a tx would hang here; the timeout guard turns
//     that hang into a failure instead of a stuck test run.
func TestStoreConcurrentReadWriteContention(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir()+"/index.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const workers = 16
	const iters = 40
	now := time.Now()

	var wg sync.WaitGroup
	errCh := make(chan error, workers*iters*2)
	start := make(chan struct{}) // release all goroutines at once to maximize contention

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range iters {
				// BM25-only write (empty Vec) — a distinct chunk per (w,i) so the
				// final count is deterministic. Goes through UpsertMany's BeginTx.
				path := fmt.Sprintf("f_w%d_i%d.go", w, i)
				row := PendingChunk{
					Path: path, Kind: "func", Name: fmt.Sprintf("Fn%d_%d", w, i),
					StartLine: 1, EndLine: 2,
					ContentSHA: path, Content: fmt.Sprintf("chunk w%d i%d", w, i),
				}
				if err := s.UpsertMany(ctx, []PendingChunk{row}, now); err != nil {
					errCh <- fmt.Errorf("UpsertMany w%d i%d: %w", w, i, err)
				}
				var n int
				if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&n); err != nil {
					errCh <- fmt.Errorf("count w%d i%d: %w", w, i, err)
				}
			}
		}(w)
	}

	close(start)

	// Bound the run so a nested-acquire deadlock fails the test instead of hanging.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent read/write did not complete in 30s — likely a single-connection deadlock (a write tx re-entered the pool?)")
	}

	close(errCh)
	var errs int
	for err := range errCh {
		errs++
		if errs <= 5 {
			t.Errorf("contention error: %v", err)
		}
	}
	if errs > 0 {
		t.Fatalf("%d contention errors under concurrent read/write (SQLITE_BUSY* must not surface with a pinned connection)", errs)
	}

	// All writes landed — the serialized path lost nothing.
	if n := chunkCount(ctx, t, s); n != workers*iters {
		t.Fatalf("final chunk count = %d, want %d — writes were dropped under contention", n, workers*iters)
	}
}
