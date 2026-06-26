package store

import "testing"

func TestScopeMatches(t *testing.T) {
	cases := []struct {
		scope, path string
		want        bool
	}{
		// glob — full path and basename
		{"internal/mcp/*_test.go", "internal/mcp/server_review_test.go", true},
		{"*_test.go", "internal/mcp/server_test.go", true},
		{"internal/mcp/*_test.go", "internal/mcp/server.go", false},
		{"*_test.go", "internal/mcp/server.go", false},
		// directory / package prefix
		{"internal/store", "internal/store/store.go", true},
		{"internal/store", "internal/store", true},
		{"internal/store", "internal/storex/x.go", false}, // prefix must be on a / boundary
		{"internal/mcp", "internal/store/store.go", false},
		// exact file
		{"cmd/dex/main.go", "cmd/dex/main.go", true},
		{"cmd/dex/main.go", "cmd/dex/other.go", false},
		// empties
		{"", "internal/store/x.go", false},
		{"internal/store", "", false},
	}
	for _, c := range cases {
		if got := scopeMatches(c.scope, c.path); got != c.want {
			t.Errorf("scopeMatches(%q, %q) = %v, want %v", c.scope, c.path, got, c.want)
		}
	}
}

func TestKnowledgeByScope(t *testing.T) {
	st, ctx := newStore(t)

	// A scoped gotcha, an unscoped note, and a differently-scoped note.
	if _, err := st.KnowledgeAddScoped(ctx, "Gotcha", "tests that shell git must scrub GIT_DIR", 0.9, "internal/mcp/*_test.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeAdd(ctx, "Fact", "the http server binds to loopback", 0.8); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeAddScoped(ctx, "Convention", "config is yaml", 0.8, "internal/store"); err != nil {
		t.Fatal(err)
	}

	// A test file in internal/mcp surfaces only the matching scoped gotcha.
	got, err := st.KnowledgeByScope(ctx, "internal/mcp/server_review_test.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 scoped match, got %d: %+v", len(got), got)
	}
	if got[0].Archetype != "Gotcha" || got[0].Scope != "internal/mcp/*_test.go" {
		t.Errorf("wrong match: %+v", got[0])
	}

	// A non-test file in internal/mcp matches nothing (glob is *_test.go).
	if none, _ := st.KnowledgeByScope(ctx, "internal/mcp/server.go", 10); len(none) != 0 {
		t.Errorf("non-test file should match no scope, got %d", len(none))
	}

	// A store file matches the prefix-scoped convention.
	if sc, _ := st.KnowledgeByScope(ctx, "internal/store/store.go", 10); len(sc) != 1 || sc[0].Archetype != "Convention" {
		t.Errorf("store file should match the internal/store convention, got %+v", sc)
	}

	// The unscoped fact never appears via scope.
	if _, err := st.KnowledgeByScope(ctx, "anything/at/all.go", 10); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeAddScopedUpdatesScope: re-adding a fact with a scope binds it.
func TestKnowledgeAddScopedUpdatesScope(t *testing.T) {
	st, ctx := newStore(t)
	body := "store tests need the sqlite_fts5 tag"
	if _, err := st.KnowledgeAdd(ctx, "Gotcha", body, 0.8); err != nil { // unscoped first
		t.Fatal(err)
	}
	if got, _ := st.KnowledgeByScope(ctx, "internal/store/store_test.go", 10); len(got) != 0 {
		t.Fatal("unscoped fact must not match by scope yet")
	}
	if _, err := st.KnowledgeAddScoped(ctx, "Gotcha", body, 0.8, "internal/store/*_test.go"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.KnowledgeByScope(ctx, "internal/store/store_test.go", 10); len(got) != 1 {
		t.Fatalf("re-adding with a scope should bind the fact, got %d", len(got))
	}
}
