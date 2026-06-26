package store

import (
	"testing"
)

func TestAssembleRelatedEmpty(t *testing.T) {
	st, ctx := newStore(t)

	// No graph, no co-access — must return nil without error.
	got, err := st.AssembleRelated(ctx, []string{"a.go"}, 10)
	if err != nil {
		t.Fatalf("AssembleRelated: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestAssembleRelatedCoAccessOnly(t *testing.T) {
	st, ctx := newStore(t)

	// Build a co-access session: a.go ↔ b.go ↔ c.go
	if err := st.SessionSetTask(ctx, "spread test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.go", "b.go", "c.go"} {
		if err := st.SessionAddFile(ctx, p, "read"); err != nil {
			t.Fatalf("SessionAddFile %s: %v", p, err)
		}
	}

	// Seed on a.go — should surface b.go and/or c.go.
	got, err := st.AssembleRelated(ctx, []string{"a.go"}, 10)
	if err != nil {
		t.Fatalf("AssembleRelated: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected neighbors via co-access, got none")
	}
	// Seed must not appear in results.
	for _, p := range got {
		if p == "a.go" {
			t.Errorf("seed a.go appeared in results: %v", got)
		}
	}
}

func TestAssembleRelatedTopNCap(t *testing.T) {
	st, ctx := newStore(t)

	if err := st.SessionSetTask(ctx, "cap test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		if err := st.SessionAddFile(ctx, p, "read"); err != nil {
			t.Fatalf("SessionAddFile %s: %v", p, err)
		}
	}

	got, err := st.AssembleRelated(ctx, []string{"a.go"}, 2)
	if err != nil {
		t.Fatalf("AssembleRelated: %v", err)
	}
	if len(got) > 2 {
		t.Errorf("expected at most 2 results, got %d: %v", len(got), got)
	}
}
