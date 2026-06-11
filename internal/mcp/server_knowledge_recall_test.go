package mcp

import (
	"context"
	"testing"
)

// TestKnowledgeSemanticRecall verifies that ctx_knowledge `add` embeds facts
// and `list` with a query recalls the most semantically relevant fact first.
// The fake embedder is deterministic per-string, so a query equal to a fact's
// body yields cosine similarity 1.0 for that fact.
func TestKnowledgeSemanticRecall(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	add := func(body string) {
		_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
			ProjectRoot: root, Action: "add", Archetype: "Fact", Body: body, Confidence: 0.8,
		})
		if err != nil || out.Status != "ok" {
			t.Fatalf("add %q: status=%q err=%v", body, out.Status, err)
		}
	}
	add("the migration uses sqlite_fts5 build tag")
	add("the http server listens on loopback")
	add("config is parsed from yaml not toml")

	// Query equal to the second fact's body must rank it first.
	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "list", K: 3,
		Query: "the http server listens on loopback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) == 0 {
		t.Fatal("no facts recalled")
	}
	if out.Facts[0].Body != "the http server listens on loopback" {
		t.Errorf("top recalled fact = %q, want the http one", out.Facts[0].Body)
	}
}
