package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// idByBody returns the row id of the fact with the given (unique) body.
// KnowledgeAdd returns revision_count, not the id, so tests look it up here.
func idByBody(t *testing.T, st *Store, ctx context.Context, body string) int64 {
	t.Helper()
	var id int64
	if err := st.db.QueryRowContext(ctx,
		`SELECT id FROM knowledge_facts WHERE body=?`, body).Scan(&id); err != nil {
		t.Fatalf("lookup id for %q: %v", body, err)
	}
	return id
}

// rewindUpdated backdates a fact's updated_at and zeroes its hit_count so the
// staleness window applies.
func rewindUpdated(t *testing.T, st *Store, ctx context.Context, id int64, d time.Duration) {
	t.Helper()
	past := time.Now().Add(-d).UnixNano()
	if _, err := st.db.ExecContext(ctx,
		`UPDATE knowledge_facts SET updated_at=?, hit_count=0 WHERE id=?`, past, id); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedExemptFromDecay(t *testing.T) {
	st, ctx := newStore(t)
	body := "a pinned fact that must not fade"
	st.KnowledgeAdd(ctx, "Fact", body, 0.9)
	if err := st.KnowledgeSetPinned(ctx, idByBody(t, st, ctx, body), true); err != nil {
		t.Fatal(err)
	}
	// Baseline GC, then rewind last_gc 30 days so decay would normally bite.
	if _, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{}); err != nil {
		t.Fatal(err)
	}
	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour).UnixNano()
	if _, err := st.db.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='knowledge_last_gc'`, thirtyDaysAgo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{}); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.KnowledgeQuery(ctx, 10)
	if len(facts) != 1 || facts[0].Confidence != 0.9 {
		t.Fatalf("pinned fact confidence=%v, want unchanged 0.9", facts)
	}
}

func TestPinnedExemptFromEviction(t *testing.T) {
	st, ctx := newStore(t)
	pinned := "low confidence but pinned forever"
	st.KnowledgeAdd(ctx, "Fact", pinned, 0.3)
	st.KnowledgeAdd(ctx, "Fact", "middle confidence disposable note here", 0.6)
	st.KnowledgeAdd(ctx, "Fact", "high confidence keeper note distinct words", 0.9)
	if err := st.KnowledgeSetPinned(ctx, idByBody(t, st, ctx, pinned), true); err != nil {
		t.Fatal(err)
	}
	// Cap of 2 over 3 facts → evict 1. The lowest-confidence fact is pinned, so
	// the disposable 0.6 fact must be the one dropped.
	res, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{MaxFacts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Evicted != 1 {
		t.Fatalf("evicted=%d, want 1", res.Evicted)
	}
	facts, _ := st.KnowledgeQuery(ctx, 10)
	var sawPinned bool
	for _, f := range facts {
		if f.Confidence == 0.3 {
			sawPinned = true
		}
	}
	if !sawPinned {
		t.Fatalf("pinned 0.3 fact was evicted; remaining=%v", facts)
	}
}

func TestPinnedExemptFromConsolidate(t *testing.T) {
	st, ctx := newStore(t)
	// Identical word-sets (jaccard 1.0) → would auto-merge, but the duplicate is
	// pinned, so both survive.
	st.KnowledgeAdd(ctx, "Gotcha", "alpha bravo charlie delta echo", 0.9)
	dup := "alpha bravo charlie delta echo!"
	st.KnowledgeAdd(ctx, "Gotcha", dup, 0.5)
	if err := st.KnowledgeSetPinned(ctx, idByBody(t, st, ctx, dup), true); err != nil {
		t.Fatal(err)
	}
	res, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 0 {
		t.Fatalf("merged=%d, want 0 (pinned duplicate must be skipped)", res.Merged)
	}
	if n, _ := st.KnowledgeCount(ctx); n != 2 {
		t.Fatalf("count=%d, want 2", n)
	}
}

func TestConsolidateAutoMergeThreshold(t *testing.T) {
	st, ctx := newStore(t)
	// Near-identical (jaccard 1.0 ≥ 0.95 default) → auto-merges.
	st.KnowledgeAdd(ctx, "Gotcha", "one two three four five", 0.9)
	st.KnowledgeAdd(ctx, "Gotcha", "one two three four five.", 0.5)
	// Sub-threshold overlap (8/9 ≈ 0.888 < 0.95) → must NOT auto-merge.
	st.KnowledgeAdd(ctx, "Fact", "alpha bravo charlie delta echo foxtrot golf hotel india", 0.9)
	st.KnowledgeAdd(ctx, "Fact", "alpha bravo charlie delta echo foxtrot golf hotel", 0.8)
	res, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 1 {
		t.Fatalf("merged=%d, want 1 (only the jaccard-1.0 Gotcha pair)", res.Merged)
	}
	if n, _ := st.KnowledgeCount(ctx); n != 3 {
		t.Fatalf("count=%d, want 3 (4 added − 1 merged)", n)
	}
}

func TestKnowledgeReviewCategories(t *testing.T) {
	t.Run("merge", func(t *testing.T) {
		st, ctx := newStore(t)
		st.KnowledgeAdd(ctx, "Gotcha", "alpha bravo charlie delta echo", 0.9)
		st.KnowledgeAdd(ctx, "Gotcha", "alpha bravo charlie delta echo!", 0.8)
		res, err := st.KnowledgeReview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Merge) != 1 || res.Total != 1 {
			t.Fatalf("merge=%d total=%d, want 1/1", len(res.Merge), res.Total)
		}
	})
	t.Run("overlap", func(t *testing.T) {
		st, ctx := newStore(t)
		// jaccard 3/5 = 0.6 → overlap band [0.5, 0.85).
		st.KnowledgeAdd(ctx, "Pattern", "alpha bravo charlie delta", 0.9)
		st.KnowledgeAdd(ctx, "Pattern", "alpha bravo charlie echo", 0.8)
		res, err := st.KnowledgeReview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Overlap) != 1 || len(res.Merge) != 0 {
			t.Fatalf("overlap=%d merge=%d, want 1/0", len(res.Overlap), len(res.Merge))
		}
	})
	t.Run("stale-and-pin-exemption", func(t *testing.T) {
		st, ctx := newStore(t)
		body := "an old never-recalled note destined to go stale"
		st.KnowledgeAdd(ctx, "Observation", body, 0.4)
		id := idByBody(t, st, ctx, body)
		rewindUpdated(t, st, ctx, id, 40*24*time.Hour)
		res, _ := st.KnowledgeReview(ctx)
		if len(res.Stale) != 1 {
			t.Fatalf("stale=%d, want 1", len(res.Stale))
		}
		if err := st.KnowledgeSetPinned(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		res, _ = st.KnowledgeReview(ctx)
		if len(res.Stale) != 0 {
			t.Fatalf("stale=%d after pin, want 0", len(res.Stale))
		}
	})
}

func TestExportRestoreRoundTripsPinned(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.db")
	st, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	body := "a permanent decision worth keeping"
	st.KnowledgeAdd(ctx, "Decision", body, 0.9)
	if err := st.KnowledgeSetPinned(ctx, idByBody(t, st, ctx, body), true); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	backups, err := ExportKnowledgeRaw(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || !backups[0].Pinned {
		t.Fatalf("export lost pinned flag: %+v", backups)
	}

	dst := filepath.Join(t.TempDir(), "dst.db")
	st2, err := Open(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if err := st2.KnowledgeRestore(ctx, backups[0]); err != nil {
		t.Fatal(err)
	}
	var pinned bool
	if err := st2.db.QueryRowContext(ctx,
		`SELECT pinned FROM knowledge_facts WHERE body=?`, backups[0].Body).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("restore lost the pinned flag")
	}
}
