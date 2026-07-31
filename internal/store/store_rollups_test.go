package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

func TestDirSummaryRoundtrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, _, ok, err := s.DirSummaryMeta(ctx, "internal/store"); err != nil || ok {
		t.Fatalf("meta on empty: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetDirSummary(ctx, "internal/store"); err != nil || ok {
		t.Fatalf("get on empty: ok=%v err=%v", ok, err)
	}

	ds := store.DirSummary{
		Path:          "internal/store",
		SourceHash:    "h1",
		PromptVersion: 1,
		Model:         "test-model",
		Summary:       "the persistence layer",
		GeneratedAt:   time.Unix(1000, 0),
	}
	if err := s.UpsertDirSummary(ctx, ds); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	h, pv, ok, err := s.DirSummaryMeta(ctx, "internal/store")
	if err != nil || !ok || h != "h1" || pv != 1 {
		t.Fatalf("meta: h=%q pv=%d ok=%v err=%v", h, pv, ok, err)
	}
	got, ok, err := s.GetDirSummary(ctx, "internal/store")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Summary != ds.Summary || got.Model != ds.Model || got.SourceHash != "h1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Upsert replaces in place (PK = path).
	ds.SourceHash = "h2"
	ds.Summary = "updated"
	if err := s.UpsertDirSummary(ctx, ds); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if h, _, _, _ = s.DirSummaryMeta(ctx, "internal/store"); h != "h2" {
		t.Fatalf("after replace: source_hash=%q, want h2", h)
	}

	// Root directory ("") is a valid key.
	if err := s.UpsertDirSummary(ctx, store.DirSummary{Path: "", SourceHash: "r", PromptVersion: 1, Model: "m", Summary: "repo", GeneratedAt: time.Unix(1, 0)}); err != nil {
		t.Fatalf("root upsert: %v", err)
	}
	if _, _, ok, _ := s.DirSummaryMeta(ctx, ""); !ok {
		t.Fatalf("root meta not found")
	}
}

func TestRollupHash(t *testing.T) {
	// Order-independent.
	if store.RollupHash([]string{"a", "b", "c"}) != store.RollupHash([]string{"c", "a", "b"}) {
		t.Fatal("RollupHash is order-dependent")
	}
	// Empty → "".
	if store.RollupHash(nil) != "" || store.RollupHash([]string{}) != "" {
		t.Fatal("empty RollupHash should be \"\"")
	}
	// A changed child flips the hash; the same set (reordered) does not.
	base := store.RollupHash([]string{"a", "b"})
	if base == store.RollupHash([]string{"a", "b2"}) {
		t.Fatal("changing a child hash must change the composite")
	}
	if base != store.RollupHash([]string{"b", "a"}) {
		t.Fatal("same set (reordered) must yield same composite")
	}
	// Shares construction with FileBodyHash: composite of two hashes equals
	// FileBodyHash over the same two chunk hashes.
	if store.RollupHash([]string{"x", "y"}) != store.FileBodyHash([]store.ChunkBody{{ContentSHA1: "y"}, {ContentSHA1: "x"}}) {
		t.Fatal("RollupHash and FileBodyHash must share the hashSorted construction")
	}
}
