package store

import (
	"testing"
	"time"
)

func TestRecencyFactor(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		at   time.Time
		want float64 // approximate
	}{
		{"today", now, 1},
		{"45 days", now.Add(-45 * 24 * time.Hour), 0.5},
		{"90 days", now.Add(-90 * 24 * time.Hour), 0},
		{"older floors at 0", now.Add(-200 * 24 * time.Hour), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := recencyFactor(c.at)
			if got < c.want-0.05 || got > c.want+0.05 {
				t.Errorf("recencyFactor(%s) = %.3f, want ~%.2f", c.name, got, c.want)
			}
		})
	}
}

func TestKnowledgeQueryVec_SemanticRanking(t *testing.T) {
	st, ctx := newStore(t)
	facts := []struct {
		body string
		vec  []float32
	}{
		{"alpha fact about X", []float32{1, 0, 0, 0}},
		{"beta fact about Y", []float32{0, 1, 0, 0}},
		{"gamma fact about Z", []float32{0, 0, 1, 0}},
	}
	for _, f := range facts {
		if _, err := st.KnowledgeAdd(ctx, "Fact", f.body, 0.8); err != nil {
			t.Fatal(err)
		}
		if err := st.KnowledgeUpsertVecByBody(ctx, f.body, f.vec); err != nil {
			t.Fatal(err)
		}
	}
	// Query closest to beta.
	got, err := st.KnowledgeQueryVec(ctx, []float32{0.1, 0.95, 0, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no facts returned")
	}
	if got[0].Body != "beta fact about Y" {
		t.Errorf("top fact = %q, want %q", got[0].Body, "beta fact about Y")
	}
}

func TestKnowledgeQueryVec_FallbackWhenNoVecs(t *testing.T) {
	st, ctx := newStore(t)
	if _, err := st.KnowledgeAdd(ctx, "Gotcha", "no embedding here", 0.9); err != nil {
		t.Fatal(err)
	}
	// No vectors stored → must fall back to salience query, not error.
	got, err := st.KnowledgeQueryVec(ctx, []float32{1, 0, 0, 0}, 5)
	if err != nil {
		t.Fatalf("expected salience fallback, got error: %v", err)
	}
	if len(got) != 1 || got[0].Body != "no embedding here" {
		t.Errorf("fallback returned %+v, want the single fact", got)
	}
}

func TestKnowledgeFactsMissingVec(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Fact", "embedded one", 0.8)
	_, _ = st.KnowledgeAdd(ctx, "Fact", "missing one", 0.8)
	if err := st.KnowledgeUpsertVecByBody(ctx, "embedded one", []float32{1, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	missing, err := st.KnowledgeFactsMissingVec(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].Body != "missing one" {
		t.Errorf("missing = %+v, want only 'missing one'", missing)
	}
}

func TestKnowledgeDelete_CascadesVec(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Fact", "to delete", 0.8)
	if err := st.KnowledgeUpsertVecByBody(ctx, "to delete", []float32{1, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	// Find id and delete.
	facts, _ := st.KnowledgeQuery(ctx, 10)
	if len(facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(facts))
	}
	if err := st.KnowledgeDelete(ctx, facts[0].ID); err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fact_vecs`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("fact_vecs has %d rows after delete, want 0 (trigger should cascade)", cnt)
	}
}
