package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestKnowledgeAddWarnsNearDuplicate covers #606: adding a note that overlaps
// an existing one surfaces it in out.Similar and warns in the hint, without
// blocking the add.
func TestKnowledgeAddWarnsNearDuplicate(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	add := func(body string) KnowledgeOutput {
		_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
			ProjectRoot: root, Action: "add", Archetype: "Gotcha", Body: body, Confidence: 0.8,
		})
		if err != nil || out.Status != "ok" {
			t.Fatalf("add %q: status=%q err=%v", body, out.Status, err)
		}
		return out
	}

	// First note — nothing similar yet.
	first := add("store tests need the sqlite_fts5 build tag or they panic")
	if len(first.Similar) != 0 {
		t.Errorf("first note should have no similar matches, got %v", first.Similar)
	}

	// Near-duplicate of the first — must be flagged.
	dup := add("store tests panic without the sqlite_fts5 build tag set")
	if len(dup.Similar) == 0 {
		t.Fatal("expected a near-duplicate warning on the second add")
	}
	if !strings.Contains(dup.Hint, "similar") {
		t.Errorf("hint should warn about similar notes, got %q", dup.Hint)
	}
	if !strings.Contains(dup.Similar[0].Body, "sqlite_fts5") {
		t.Errorf("similar note should be the sqlite_fts5 one, got %q", dup.Similar[0].Body)
	}

	// An unrelated note triggers no warning.
	unrelated := add("the weather today is sunny and warm outside")
	if len(unrelated.Similar) != 0 {
		t.Errorf("unrelated note should have no similar matches, got %v", unrelated.Similar)
	}
}
