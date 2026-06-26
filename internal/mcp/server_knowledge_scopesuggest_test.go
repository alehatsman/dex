package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSuggestScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"internal/store/store.go", "main.go"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name, body, want string
	}{
		{"real file path", "the cache lives in internal/store/store.go and is tricky", "internal/store/store.go"},
		{"directory path", "everything under internal/store shares the same lock", "internal/store"},
		{"glob as-is", "tests matching *_test.go must scrub GIT_DIR", "*_test.go"},
		{"glob with dir", "internal/mcp/*_test.go all need a fake embedder", "internal/mcp/*_test.go"},
		{"external path not suggested", "unlike github.com/foo/bar.go we vendor nothing", ""},
		{"nonexistent path not suggested", "the old internal/legacy/gone.go was removed", ""},
		{"bare word not suggested", "the server binds to loopback by default", ""},
		{"bare filename too ambiguous", "see store.go for details", ""},
		{"prose with version not suggested", "requires go 1.21 and module v2.0", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := suggestScope(root, c.body); got != c.want {
				t.Errorf("suggestScope(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestKnowledgeAddSuggestsScope: the add response carries a scope_suggestion
// when the unscoped note names a real project file, and none when scoped.
func TestKnowledgeAddSuggestsScope(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "server.go"), "package main\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	// Unscoped note naming a real file → suggestion.
	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "server.go has a subtle init-order bug — see internal nothing, just server.go",
	})
	if err != nil || out.Status != "ok" {
		t.Fatalf("add: status=%q err=%v", out.Status, err)
	}
	if out.ScopeSuggestion != "" {
		// "server.go" is a bare filename (no slash) → too ambiguous, no suggestion.
		t.Errorf("bare filename should not be suggested, got %q", out.ScopeSuggestion)
	}

	// A note naming the file with a path → suggestion (use a subdir file).
	writeFile(t, filepath.Join(projDir, "pkg", "h.go"), "package pkg\n")
	_, out2, _ := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "the handler in pkg/h.go must be called before init",
	})
	if out2.ScopeSuggestion != "pkg/h.go" {
		t.Errorf("path-qualified file should be suggested, got %q", out2.ScopeSuggestion)
	}

	// Already scoped → no suggestion.
	_, out3, _ := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha", Scope: "pkg",
		Body: "another note about pkg/h.go behavior",
	})
	if out3.ScopeSuggestion != "" {
		t.Errorf("already-scoped add should not suggest, got %q", out3.ScopeSuggestion)
	}
}
