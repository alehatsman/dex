package store

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newKnowledgeDB creates a minimal in-memory SQLite DB with only the
// knowledge_facts table — avoids the fts5 dependency in the full migration.
func newKnowledgeDB(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_journal=WAL")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE knowledge_facts (
		   id         INTEGER PRIMARY KEY AUTOINCREMENT,
		   archetype  TEXT NOT NULL DEFAULT 'Observation',
		   body       TEXT NOT NULL,
		   confidence REAL NOT NULL DEFAULT 0.8,
		   created_at INTEGER NOT NULL,
		   updated_at INTEGER NOT NULL,
		   hit_count      INTEGER NOT NULL DEFAULT 0,
		   revision_count INTEGER NOT NULL DEFAULT 0,
		   UNIQUE(body)
		)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db}, context.Background()
}

func TestKnowledgeAddAndQuery(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	rev, err := st.KnowledgeAdd(ctx, "Gotcha", "never call Foo after Bar", 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 0 {
		t.Errorf("first insert: want revision 0, got %d", rev)
	}

	facts, err := st.KnowledgeQuery(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.Body != "never call Foo after Bar" {
		t.Errorf("body mismatch: %q", f.Body)
	}
	if f.Confidence != 0.9 {
		t.Errorf("confidence: got %v, want 0.9", f.Confidence)
	}
	if f.Salience <= 0 {
		t.Errorf("salience must be > 0, got %v", f.Salience)
	}
}

func TestKnowledgeAddDedup(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	if _, err := st.KnowledgeAdd(ctx, "Gotcha", "same body", 0.7); err != nil {
		t.Fatal(err)
	}
	rev, err := st.KnowledgeAdd(ctx, "Gotcha", "same body", 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Errorf("second insert: want revision 1, got %d", rev)
	}

	facts, err := st.KnowledgeQuery(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Errorf("dedup failed: want 1 fact, got %d", len(facts))
	}
	// confidence should be promoted to the higher value
	if facts[0].Confidence != 0.9 {
		t.Errorf("confidence not promoted: got %v, want 0.9", facts[0].Confidence)
	}
}

func TestKnowledgeSalienceDecayRanking(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	// stale: high raw confidence but old
	staleNs := time.Now().Add(-60 * 24 * time.Hour).UnixNano() // 60 days ago
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count)
		   VALUES('Observation','stale high-conf fact',0.95,?,?,0,0)`,
		staleNs, staleNs)
	if err != nil {
		t.Fatal(err)
	}

	// fresh: lower raw confidence but recent
	freshNs := time.Now().UnixNano()
	_, err = st.db.ExecContext(ctx,
		`INSERT INTO knowledge_facts(archetype, body, confidence, created_at, updated_at, hit_count, revision_count)
		   VALUES('Architecture','fresh lower-conf fact',0.75,?,?,0,0)`,
		freshNs, freshNs)
	if err != nil {
		t.Fatal(err)
	}

	facts, err := st.KnowledgeQuery(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 facts, got %d", len(facts))
	}

	// Verify decay: stale fact salience must be significantly reduced.
	// At 60 days with rate 0.01: exp(-0.01*60) ≈ 0.549
	// stale salience ≈ 0.95 * 1.0 * 0.549 ≈ 0.521
	// fresh salience ≈ 0.75 * 1.5 * ~1.0 ≈ 1.125 (Architecture weight 1.5)
	// So fresh should rank first.
	if facts[0].Body != "fresh lower-conf fact" {
		t.Errorf("expected fresh fact to rank first after decay, got %q (salience=%.3f) over %q (salience=%.3f)",
			facts[0].Body, facts[0].Salience, facts[1].Body, facts[1].Salience)
	}

	// Verify the stale fact's salience is actually decayed (not just confidence*weight).
	expectedStaleMax := 0.95 * 1.0 // without decay
	if facts[1].Salience >= expectedStaleMax {
		t.Errorf("stale fact salience not decayed: got %.4f, want < %.4f", facts[1].Salience, expectedStaleMax)
	}
}

func TestKnowledgeSalienceDecayMath(t *testing.T) {
	// Unit-test the decay formula directly.
	days := 30.0
	decay := math.Exp(-salinceDecayRate * days)
	// At 30 days with rate 0.01: exp(-0.3) ≈ 0.7408
	if decay < 0.73 || decay > 0.76 {
		t.Errorf("decay at 30 days = %.4f, want ~0.74", decay)
	}

	days = 0
	decay = math.Exp(-salinceDecayRate * days)
	if decay != 1.0 {
		t.Errorf("decay at 0 days = %.4f, want 1.0", decay)
	}
}

func TestKnowledgeDelete(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	if _, err := st.KnowledgeAdd(ctx, "Gotcha", "to be deleted", 0.8); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.KnowledgeQuery(ctx, 10)
	id := facts[0].ID

	if err := st.KnowledgeDelete(ctx, id); err != nil {
		t.Fatal(err)
	}
	facts, _ = st.KnowledgeQuery(ctx, 10)
	if len(facts) != 0 {
		t.Errorf("want 0 facts after delete, got %d", len(facts))
	}

	// delete non-existent
	if err := st.KnowledgeDelete(ctx, 99999); err == nil {
		t.Error("expected error deleting non-existent fact")
	}
}

func TestKnowledgeBump(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	if _, err := st.KnowledgeAdd(ctx, "Pattern", "bump me", 0.7); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.KnowledgeQuery(ctx, 10)
	id := facts[0].ID
	if facts[0].HitCount != 0 {
		t.Errorf("initial hit_count want 0, got %d", facts[0].HitCount)
	}

	if err := st.KnowledgeBump(ctx, id); err != nil {
		t.Fatal(err)
	}
	facts, _ = st.KnowledgeQuery(ctx, 10)
	if facts[0].HitCount != 1 {
		t.Errorf("after bump want hit_count=1, got %d", facts[0].HitCount)
	}
}

func TestKnowledgeQueryTopK(t *testing.T) {
	st, ctx := newKnowledgeDB(t)

	for i := 0; i < 10; i++ {
		body := "fact " + string(rune('A'+i))
		if _, err := st.KnowledgeAdd(ctx, "Observation", body, float64(i+1)/10.0); err != nil {
			t.Fatal(err)
		}
	}

	facts, err := st.KnowledgeQuery(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Errorf("want 3 facts, got %d", len(facts))
	}
	// all fresh so highest confidence should win
	if facts[0].Confidence < facts[1].Confidence {
		t.Errorf("not sorted by salience: facts[0].Conf=%.2f < facts[1].Conf=%.2f",
			facts[0].Confidence, facts[1].Confidence)
	}
}
