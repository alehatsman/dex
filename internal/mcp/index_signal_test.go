// Copyright 2026 Aleh Atsman
//
// Regression test for #531: query surfaces must turn the store's indexing
// marker into a caller-facing notice so partial results aren't trusted.

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

func TestIndexingNotice(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Not indexing -> no notice.
	if indexing, note := indexingNotice(ctx, st); indexing || note != "" {
		t.Fatalf("idle store: got (%v, %q), want (false, \"\")", indexing, note)
	}

	// Indexing -> notice naming the rebuild.
	if err := st.SetIndexing(ctx, time.Now()); err != nil {
		t.Fatalf("SetIndexing: %v", err)
	}
	indexing, note := indexingNotice(ctx, st)
	if !indexing {
		t.Fatal("expected indexing=true while marker is set")
	}
	if !strings.Contains(note, "rebuild in progress") || !strings.Contains(note, "partial") {
		t.Errorf("notice should describe the partial rebuild, got: %q", note)
	}
}
