package index

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/store"
)

// TestDrainDurabilityWindow verifies that DrainPendingSummariesBatch with
// max=0 processes at most durabilityWindow rows per call, not the entire queue.
//
// Strategy: enqueue durabilityWindow+5 file_summary rows pointing to
// nonexistent files. processFileSummary treats missing files as stale and
// drops them without calling chat — so the test needs no real AI backend and
// runs fast. After one batch call, remaining must equal 5.
func TestDrainDurabilityWindow(t *testing.T) {
	cc, chatCalls := countingChat(t)
	ix, _ := testIndexer(t, Options{Chat: cc})

	total := durabilityWindow + 5
	ctx := context.Background()
	now := time.Now()
	for i := range total {
		if err := ix.Store.EnqueuePendingSummary(ctx, store.PendingSummary{
			Path:       fmt.Sprintf("nonexistent/file_%04d.go", i),
			Kind:       chunk.KindFileSummary,
			ContentSHA: fmt.Sprintf("sha%04d", i),
		}, now); err != nil {
			t.Fatalf("enqueue row %d: %v", i, err)
		}
	}

	generated, remaining, err := ix.DrainPendingSummariesBatch(ctx, 0)
	if err != nil {
		t.Fatalf("DrainPendingSummariesBatch: %v", err)
	}

	// All processed rows were stale (file not found) → 0 real summaries generated.
	if generated != 0 {
		t.Errorf("generated=%d, want 0 (all rows are stale)", generated)
	}
	// Only durabilityWindow rows should have been consumed.
	if remaining != 5 {
		t.Errorf("remaining=%d, want 5 (only %d of %d rows processed per window)",
			remaining, durabilityWindow, total)
	}
	// Chat must not have been called: stale rows are dropped before summarize.
	if n := chatCalls.Load(); n != 0 {
		t.Errorf("chat called %d times, want 0 (stale rows skip chat)", n)
	}
}

// TestDrainDurabilityWindowExplicitMax verifies that an explicit max overrides
// the durability window — callers that pass max=10 still get exactly 10 rows.
func TestDrainDurabilityWindowExplicitMax(t *testing.T) {
	cc, _ := countingChat(t)
	ix, _ := testIndexer(t, Options{Chat: cc})

	ctx := context.Background()
	now := time.Now()
	for i := range 20 {
		if err := ix.Store.EnqueuePendingSummary(ctx, store.PendingSummary{
			Path:       fmt.Sprintf("nonexistent/file_%04d.go", i),
			Kind:       chunk.KindFileSummary,
			ContentSHA: fmt.Sprintf("sha%04d", i),
		}, now); err != nil {
			t.Fatalf("enqueue row %d: %v", i, err)
		}
	}

	_, remaining, err := ix.DrainPendingSummariesBatch(ctx, 10)
	if err != nil {
		t.Fatalf("DrainPendingSummariesBatch: %v", err)
	}
	if remaining != 10 {
		t.Errorf("remaining=%d, want 10 (explicit max=10 respected)", remaining)
	}
}
