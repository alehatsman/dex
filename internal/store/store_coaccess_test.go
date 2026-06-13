package store

import (
	"testing"
	"time"
)

func TestRecordCoAccessAndNeighbors(t *testing.T) {
	st, ctx := newStore(t)

	// No session yet — RecordCoAccess must be a no-op (no error).
	if err := st.RecordCoAccess(ctx, "a.go"); err != nil {
		t.Fatalf("RecordCoAccess before session: %v", err)
	}

	// Seed a session with 3 files so the working set is populated.
	if err := st.SessionSetTask(ctx, "test task"); err != nil {
		t.Fatalf("SessionSetTask: %v", err)
	}
	for _, p := range []string{"x.go", "y.go", "z.go"} {
		if err := st.SessionAddFile(ctx, p, "read"); err != nil {
			t.Fatalf("SessionAddFile %s: %v", p, err)
		}
	}

	// Now record access to a.go; it should have been star-associated
	// against x.go, y.go, z.go by SessionAddFile already, but let's
	// also call RecordCoAccess directly for b.go:
	if err := st.RecordCoAccess(ctx, "b.go"); err != nil {
		t.Fatalf("RecordCoAccess b.go: %v", err)
	}

	// CoAccessNeighbors from [x.go] should find some neighbors.
	neighbors, err := st.CoAccessNeighbors(ctx, []string{"x.go"}, 10)
	if err != nil {
		t.Fatalf("CoAccessNeighbors: %v", err)
	}
	if len(neighbors) == 0 {
		t.Fatal("expected neighbors for x.go, got none")
	}

	// Neighbors must not include x.go itself.
	for _, n := range neighbors {
		if n == "x.go" {
			t.Error("neighbor list contains the seed x.go")
		}
	}
}

func TestRecordCoAccessLTPFormula(t *testing.T) {
	st, ctx := newStore(t)

	if err := st.SessionSetTask(ctx, "ltp test"); err != nil {
		t.Fatal(err)
	}
	// Seed the session working set.
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
	// First association: edge (a.go, b.go) should be created with weight ≈ 1.0
	// (plus whatever SessionAddFile did for b.go).
	if err := st.RecordCoAccess(ctx, "b.go"); err != nil {
		t.Fatal(err)
	}

	var w1 float64
	if err := st.db.QueryRowContext(ctx,
		`SELECT weight FROM co_access_edges WHERE (src_path='a.go' AND dst_path='b.go')
		      OR (src_path='b.go' AND dst_path='a.go') LIMIT 1`).Scan(&w1); err != nil {
		t.Fatalf("reading weight: %v", err)
	}
	if w1 < 0.9 || w1 > 2.1 {
		t.Errorf("unexpected initial weight %.3f", w1)
	}

	// Second access: reinforce the edge. Weight should increase (still well below cap).
	if err := st.RecordCoAccess(ctx, "b.go"); err != nil {
		t.Fatal(err)
	}
	var w2 float64
	if err := st.db.QueryRowContext(ctx,
		`SELECT weight FROM co_access_edges WHERE (src_path='a.go' AND dst_path='b.go')
		      OR (src_path='b.go' AND dst_path='a.go') LIMIT 1`).Scan(&w2); err != nil {
		t.Fatalf("reading weight 2: %v", err)
	}
	if w2 < w1 {
		t.Errorf("weight did not increase on second access: %.3f → %.3f", w1, w2)
	}
	if w2 > coAccessMaxWeight+0.01 {
		t.Errorf("weight %.3f exceeds cap %.1f", w2, coAccessMaxWeight)
	}
}

func TestCoAccessDisabled(t *testing.T) {
	db := t.TempDir() + "/disabled.db"
	ctx := t.Context()
	st, err := OpenWith(ctx, db, Options{InfraOptions: InfraOptions{DisableCoAccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.SessionSetTask(ctx, "disabled test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "b.go", "read"); err != nil {
		t.Fatal(err)
	}

	// No edges should exist.
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM co_access_edges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("DisableCoAccess=true but found %d edges", count)
	}
}

func TestPruneCoAccess(t *testing.T) {
	st, ctx := newStore(t)

	nowNs := time.Now().UnixNano()

	// Insert a very-old edge (epoch=1) — decayed far below threshold.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO co_access_edges(src_path, dst_path, weight, reinforced_at) VALUES('p.go','q.go',0.001,1)`); err != nil {
		t.Fatal(err)
	}
	// Insert a healthy edge reinforced right now — negligible decay.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO co_access_edges(src_path, dst_path, weight, reinforced_at) VALUES('m.go','n.go',9.0,?)`,
		nowNs); err != nil {
		t.Fatal(err)
	}

	if err := st.PruneCoAccess(ctx); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM co_access_edges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("want 1 edge after prune, got %d", count)
	}
}
