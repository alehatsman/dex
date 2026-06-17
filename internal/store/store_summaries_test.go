package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

func TestFileSummaryRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, _, ok, err := s.FileSummaryMeta(ctx, "a.go"); err != nil || ok {
		t.Fatalf("expected no meta for unknown path, got ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetFileSummary(ctx, "a.go"); err != nil || ok {
		t.Fatalf("expected no summary for unknown path, got ok=%v err=%v", ok, err)
	}

	now := time.Unix(1_700_000_000, 0)
	fs := store.FileSummary{
		Path: "a.go", SourceHash: "h1", PromptVersion: 1,
		Model: "test-model", Summary: "does a thing", GeneratedAt: now,
	}
	if err := s.UpsertFileSummary(ctx, fs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h, pv, ok, err := s.FileSummaryMeta(ctx, "a.go")
	if err != nil || !ok || h != "h1" || pv != 1 {
		t.Fatalf("meta = (%q,%d,%v,%v), want (h1,1,true,nil)", h, pv, ok, err)
	}
	got, ok, err := s.GetFileSummary(ctx, "a.go")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Summary != "does a thing" || got.Model != "test-model" || got.SourceHash != "h1" {
		t.Fatalf("get mismatch: %+v", got)
	}
	if !got.GeneratedAt.Equal(now) {
		t.Fatalf("generated_at = %v, want %v", got.GeneratedAt, now)
	}

	// Upsert replaces in place (PRIMARY KEY on path).
	fs.SourceHash, fs.Summary, fs.PromptVersion = "h2", "does another thing", 2
	if err := s.UpsertFileSummary(ctx, fs); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	h, pv, _, _ = s.FileSummaryMeta(ctx, "a.go")
	if h != "h2" || pv != 2 {
		t.Fatalf("after update meta = (%q,%d), want (h2,2)", h, pv)
	}
}

func TestChunkBodiesByPathAndHash(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	rows := []store.PendingChunk{
		{Path: "a.go", Kind: "func", Name: "B", StartLine: 20, EndLine: 25, ContentSHA: "sB", Content: "body B", Vec: []float32{0.1, 0.2}},
		{Path: "a.go", Kind: "func", Name: "A", StartLine: 1, EndLine: 5, ContentSHA: "sA", Content: "body A", Vec: []float32{0.3, 0.4}},
		{Path: "other.go", Kind: "func", Name: "C", StartLine: 1, EndLine: 2, ContentSHA: "sC", Content: "body C", Vec: []float32{0.5, 0.6}},
	}
	if err := s.UpsertMany(ctx, rows, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("upsert chunks: %v", err)
	}

	bodies, err := s.ChunkBodiesByPath(ctx, "a.go")
	if err != nil {
		t.Fatalf("ChunkBodiesByPath: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d bodies, want 2", len(bodies))
	}
	// Ordered by start line: A (1) before B (20).
	if bodies[0].Content != "body A" || bodies[1].Content != "body B" {
		t.Fatalf("bad order: %q then %q", bodies[0].Content, bodies[1].Content)
	}

	// FileBodyHash is stable and order-independent over chunk hashes.
	h1 := store.FileBodyHash(bodies)
	reversed := []store.ChunkBody{bodies[1], bodies[0]}
	if h2 := store.FileBodyHash(reversed); h1 != h2 {
		t.Fatalf("hash not order-independent: %q vs %q", h1, h2)
	}
	if store.FileBodyHash(nil) != "" {
		t.Fatal("empty input should hash to empty string")
	}
	// A body change flips the hash.
	changed := []store.ChunkBody{{ContentSHA1: "sA"}, {ContentSHA1: "sB-changed"}}
	if store.FileBodyHash(changed) == h1 {
		t.Fatal("hash should change when a chunk body hash changes")
	}
}
