package store

import (
	"testing"
	"time"
)

func TestCooccurNeighborBonus_BoostsHebbianNeighbors(t *testing.T) {
	st, ctx := newStore(t)

	// Session touches only a.go — b.go and c.go must NOT be session-touched
	// so the bonus we observe is the spreading-activation one, not the
	// direct sessionProximityBonus. Insert the cooccur edges directly so
	// b.go and c.go never enter sess.Files.
	if err := st.SessionSetTask(ctx, "spreading bonus"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	for _, e := range []struct {
		src, dst string
		w        float64
	}{
		{"a.go", "b.go", coAccessMaxWeight}, // strongest tie
		{"a.go", "c.go", 1.0},               // weaker tie
	} {
		src, dst := canonicalize(e.src, e.dst)
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO co_access_edges(src_path, dst_path, weight, reinforced_at) VALUES(?, ?, ?, ?)`,
			src, dst, e.w, now); err != nil {
			t.Fatal(err)
		}
	}

	// pathFor matches the candidate-pool shape applyProximityBonus sees.
	// id=10 → b.go (strong neighbor); id=20 → c.go (weak); id=30 → unrelated.
	pathFor := map[int64]string{
		10: "b.go",
		20: "c.go",
		30: "unrelated.go",
	}
	bonus := st.cooccurNeighborBonus(ctx, pathFor)

	if bonus[10] == 0 {
		t.Errorf("strong neighbor b.go: want non-zero bonus, got 0")
	}
	if bonus[20] == 0 {
		t.Errorf("weak neighbor c.go: want non-zero bonus, got 0")
	}
	if bonus[30] != 0 {
		t.Errorf("unrelated.go has no edge; want 0, got %v", bonus[30])
	}
	// Weight ordering must be preserved: stronger edge → larger bonus.
	if bonus[10] <= bonus[20] {
		t.Errorf("stronger edge should produce larger bonus: b.go=%v, c.go=%v", bonus[10], bonus[20])
	}
	// Even at maxWeight, the spreading bonus stays strictly under the direct
	// session-proximity bonus (1/rrfK) due to alpha-scaling.
	direct := float32(1) / float32(rrfK)
	if bonus[10] >= direct {
		t.Errorf("spreading bonus at peak (%v) should be < direct proximity (%v)", bonus[10], direct)
	}
}

func TestCooccurNeighborBonus_DisabledShortCircuits(t *testing.T) {
	db := t.TempDir() + "/disabled.db"
	ctx := t.Context()
	st, err := OpenWith(ctx, db, Options{InfraOptions: InfraOptions{DisableCoAccess: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.SessionSetTask(ctx, "disabled bonus"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "b.go", "read"); err != nil {
		t.Fatal(err)
	}

	bonus := st.cooccurNeighborBonus(ctx, map[int64]string{1: "b.go"})
	if len(bonus) != 0 {
		t.Errorf("DisableCoAccess=true should short-circuit; got %v", bonus)
	}
}

func TestCooccurNeighborBonus_NoSessionNoBonus(t *testing.T) {
	st, ctx := newStore(t)
	// No session set up — bonus must be empty.
	bonus := st.cooccurNeighborBonus(ctx, map[int64]string{1: "anything.go"})
	if len(bonus) != 0 {
		t.Errorf("no active session; want empty bonus, got %v", bonus)
	}
}

func TestCoAccessNeighborsWeighted_ReturnsRawWeight(t *testing.T) {
	st, ctx := newStore(t)
	if err := st.SessionSetTask(ctx, "weighted neighbors"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}
	if err := st.SessionAddFile(ctx, "b.go", "read"); err != nil {
		t.Fatal(err)
	}

	wn, err := st.CoAccessNeighborsWeighted(ctx, []string{"a.go"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(wn) == 0 {
		t.Fatal("want at least one weighted neighbor for a.go")
	}
	for _, n := range wn {
		if n.Weight <= 0 || n.Weight > coAccessMaxWeight {
			t.Errorf("weight out of range: %v (cap=%v)", n.Weight, coAccessMaxWeight)
		}
		if n.Path == "a.go" {
			t.Error("seed must not appear in its own neighbor list")
		}
	}
}
