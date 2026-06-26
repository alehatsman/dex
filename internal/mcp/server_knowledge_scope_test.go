package mcp

import (
	"context"
	"testing"
)

// TestKnowledgeListByScope covers #653: list with a scope returns only the
// notes whose scope binds that path — what would surface on touching it.
func TestKnowledgeListByScope(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	add := func(body, scope string) {
		_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
			ProjectRoot: root, Action: "add", Archetype: "Gotcha", Body: body, Confidence: 0.8, Scope: scope,
		})
		if err != nil || out.Status != "ok" {
			t.Fatalf("add %q: status=%q err=%v", body, out.Status, err)
		}
	}
	add("tests here must scrub GIT_DIR", "internal/mcp/*_test.go")
	add("config is yaml", "internal/store")
	add("the server binds loopback", "") // unscoped

	list := func(scope string) []KnowledgeFactOutput {
		_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
			ProjectRoot: root, Action: "list", Scope: scope,
		})
		if err != nil || out.Status != "ok" {
			t.Fatalf("list scope=%q: status=%q err=%v", scope, out.Status, err)
		}
		return out.Facts
	}

	// A test file in internal/mcp matches only the test-glob note.
	got := list("internal/mcp/server_test.go")
	if len(got) != 1 || got[0].Scope != "internal/mcp/*_test.go" {
		t.Fatalf("scope=test file: want 1 (the *_test.go gotcha), got %d: %+v", len(got), got)
	}

	// A store file matches only the store-prefix note.
	if g := list("internal/store/store.go"); len(g) != 1 || g[0].Scope != "internal/store" {
		t.Fatalf("scope=store file: want 1 (internal/store), got %d: %+v", len(g), g)
	}

	// A path with no scoped note matches nothing (the unscoped note never shows).
	if g := list("cmd/dex/main.go"); len(g) != 0 {
		t.Errorf("scope=unmatched path: want 0, got %d: %+v", len(g), g)
	}

	// No scope → falls back to salience list (all 3 notes, unfiltered).
	if g := list(""); len(g) != 3 {
		t.Errorf("no scope: want all 3 notes by salience, got %d", len(g))
	}
}
