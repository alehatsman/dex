package store

import "testing"

func TestKnowledgeSimilar(t *testing.T) {
	st, ctx := newStore(t)

	seed := []string{
		"store tests need the sqlite_fts5 build tag or they panic",
		"the config file lives at dot dex slash config dot yml",
		"reindex drops the whole database and rebuilds from scratch",
	}
	for _, b := range seed {
		if _, err := st.KnowledgeAdd(ctx, "Gotcha", b, 0.8); err != nil {
			t.Fatal(err)
		}
	}

	// A near-duplicate of the first seed (shares most content words).
	cand := "store tests panic without the sqlite_fts5 build tag set"
	got, err := st.KnowledgeSimilar(ctx, cand, 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected a near-duplicate match")
	}
	if got[0].Body != seed[0] {
		t.Errorf("top match = %q, want the sqlite_fts5 note", got[0].Body)
	}
	if got[0].Similarity <= 0 || got[0].Similarity > 1 {
		t.Errorf("similarity out of range: %v", got[0].Similarity)
	}

	// An unrelated body matches nothing above threshold.
	none, err := st.KnowledgeSimilar(ctx, "the weather today is sunny and warm outside", 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("unrelated body should match nothing, got %d", len(none))
	}

	// Byte-identical body is excluded (that's the upsert path, not a near-dup).
	exact, err := st.KnowledgeSimilar(ctx, seed[0], 0.3, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, sf := range exact {
		if sf.Body == seed[0] {
			t.Errorf("exact body must be excluded from similar results")
		}
	}

	// max caps the result count.
	capped, err := st.KnowledgeSimilar(ctx, cand, 0.0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) > 2 {
		t.Errorf("max=2 not honored: got %d", len(capped))
	}
}
