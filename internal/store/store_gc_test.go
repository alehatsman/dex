package store

import (
	"fmt"
	"testing"
	"time"
)

func TestJaccard(t *testing.T) {
	a := wordSet("the migration uses sqlite fts5 build tag")
	b := wordSet("the migration uses sqlite fts5 build flag")
	if j := jaccard(a, b); j < 0.7 {
		t.Errorf("near-identical sentences jaccard=%.2f, want high", j)
	}
	c := wordSet("completely different unrelated sentence here")
	if j := jaccard(a, c); j > 0.2 {
		t.Errorf("unrelated sentences jaccard=%.2f, want low", j)
	}
}

func TestKnowledgeGC_ConsolidatesSimilar(t *testing.T) {
	st, ctx := newStore(t)
	// Two facts with identical word-sets (same archetype) + one distinct. The
	// default auto-merge threshold is 0.95 (#633), so only near-identical bodies
	// consolidate — the trailing period differs the body text but not the words.
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "store tests need the sqlite_fts5 build tag", 0.9)
	_, _ = st.KnowledgeAdd(ctx, "Gotcha", "store tests need the sqlite_fts5 build tag.", 0.7)
	_, _ = st.KnowledgeAdd(ctx, "Decision", "config is parsed from yaml", 0.8)

	res, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 1 {
		t.Errorf("merged=%d, want 1", res.Merged)
	}
	if res.Remaining != 2 {
		t.Errorf("remaining=%d, want 2", res.Remaining)
	}
	// The surviving Gotcha keeps the higher confidence.
	facts, _ := st.KnowledgeQuery(ctx, 10)
	for _, f := range facts {
		if f.Archetype == "Gotcha" && f.Confidence < 0.9 {
			t.Errorf("merged Gotcha confidence=%.2f, want ≥0.9 (keep the higher)", f.Confidence)
		}
	}
}

func TestKnowledgeGC_Evicts(t *testing.T) {
	st, ctx := newStore(t)
	words := []string{"authentication", "rendering", "migration", "scheduler", "telemetry",
		"compression", "embedding", "reranking", "watcher", "indexing", "throttling", "consolidation"}
	for i, w := range words {
		body := "the " + w + " subsystem owns its own configuration block"
		conf := 0.3 + float64(i)*0.05
		if _, err := st.KnowledgeAdd(ctx, "Fact", body, conf); err != nil {
			t.Fatal(err)
		}
	}
	res, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{MaxFacts: 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.Remaining != 5 {
		t.Errorf("remaining=%d, want 5 after eviction", res.Remaining)
	}
	if res.Evicted != 7 {
		t.Errorf("evicted=%d, want 7", res.Evicted)
	}
	// The lowest-confidence facts must be gone; the top one survives.
	facts, _ := st.KnowledgeQuery(ctx, 50)
	for _, f := range facts {
		if f.Confidence < 0.5 {
			t.Errorf("low-confidence fact %.2f survived eviction", f.Confidence)
		}
	}
}

func TestKnowledgeGC_DecayIsIncremental(t *testing.T) {
	st, ctx := newStore(t)
	_, _ = st.KnowledgeAdd(ctx, "Fact", "a fact that will age", 0.9)

	// First GC records the baseline only — no decay, no error.
	if _, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{}); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.KnowledgeQuery(ctx, 10)
	if len(facts) != 1 || facts[0].Confidence != 0.9 {
		t.Fatalf("after baseline GC confidence=%v, want unchanged 0.9", facts)
	}

	// Simulate time passing by rewinding last_gc 30 days into the past.
	thirtyDaysAgo := fmt.Sprintf("%d", time.Now().Add(-30*24*time.Hour).UnixNano())
	if _, err := st.db.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='knowledge_last_gc'`, thirtyDaysAgo); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeGC(ctx, KnowledgeGCConfig{}); err != nil {
		t.Fatal(err)
	}
	facts, _ = st.KnowledgeQuery(ctx, 10)
	if facts[0].Confidence >= 0.9 {
		t.Errorf("confidence=%.4f after 30d, want decayed below 0.9", facts[0].Confidence)
	}
	if facts[0].Confidence < 0.05 {
		t.Errorf("confidence=%.4f below floor 0.05", facts[0].Confidence)
	}
}
