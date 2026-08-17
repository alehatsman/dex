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

// TestStoreConcurrentReadWriteContention hammers one *Store with concurrent
// transactional writes (KnowledgeAdd → BeginTx → addInTx) and reads
// (KnowledgeCount / KnowledgeQuery). Two things are under test:
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

	var wg sync.WaitGroup
	errCh := make(chan error, workers*iters*3)
	start := make(chan struct{}) // release all goroutines at once to maximize contention

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := range iters {
				if _, err := s.KnowledgeAdd(ctx, "note", fmt.Sprintf("fact w%d i%d", w, i), 0.5); err != nil {
					errCh <- fmt.Errorf("KnowledgeAdd w%d i%d: %w", w, i, err)
				}
				if _, err := s.KnowledgeCount(ctx); err != nil {
					errCh <- fmt.Errorf("KnowledgeCount w%d i%d: %w", w, i, err)
				}
				if _, err := s.KnowledgeQuery(ctx, 5); err != nil {
					errCh <- fmt.Errorf("KnowledgeQuery w%d i%d: %w", w, i, err)
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
	if n, err := s.KnowledgeCount(ctx); err != nil {
		t.Fatalf("final count: %v", err)
	} else if n != workers*iters {
		t.Fatalf("final fact count = %d, want %d — writes were dropped under contention", n, workers*iters)
	}
}
