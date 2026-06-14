// Copyright 2026 Aleh Atsman
//
// Regression test for #531: the store must expose a cross-process
// "indexing in progress" marker so readers don't serve a half-rebuilt
// index as authoritative.

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

func TestIndexingMarkerLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Fresh index: not in progress.
	if in, _ := s.IndexingInProgress(ctx); in {
		t.Fatal("fresh store should not report indexing in progress")
	}

	// Marked: in progress, start time round-trips.
	start := time.Now()
	if err := s.SetIndexing(ctx, start); err != nil {
		t.Fatalf("SetIndexing: %v", err)
	}
	in, got := s.IndexingInProgress(ctx)
	if !in {
		t.Fatal("after SetIndexing, expected in-progress=true")
	}
	if got.Unix() != start.Unix() {
		t.Errorf("start time = %v, want ~%v", got, start)
	}

	// Cleared: not in progress.
	if err := s.ClearIndexing(ctx); err != nil {
		t.Fatalf("ClearIndexing: %v", err)
	}
	if in, _ := s.IndexingInProgress(ctx); in {
		t.Error("after ClearIndexing, expected in-progress=false")
	}
}

func TestIndexingMarkerSelfHealsStale(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// A marker older than IndexingStaleAfter is treated as a crashed indexer.
	old := time.Now().Add(-store.IndexingStaleAfter - time.Minute)
	if err := s.SetIndexing(ctx, old); err != nil {
		t.Fatalf("SetIndexing: %v", err)
	}
	if in, _ := s.IndexingInProgress(ctx); in {
		t.Error("a stale indexing marker should report not-in-progress")
	}
}
