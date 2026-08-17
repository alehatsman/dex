package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestSharedReferents covers the #167 Part 3 overlap primitive: two bodies
// "share" a referent when they name the same file (line numbers collapse) or
// the same symbol, and nothing when they don't.
func TestSharedReferents(t *testing.T) {
	cases := []struct {
		name       string
		a, b       string
		wantShared []string
	}{
		{
			name:       "same file, different lines",
			a:          "internal/server.go:42 sets the retry ceiling",
			b:          "grpc deadline lives at internal/server.go:88",
			wantShared: []string{"internal/server.go"},
		},
		{
			name:       "shared dotted symbol",
			a:          "store.KnowledgeAdd trims trailing whitespace",
			b:          "call store.KnowledgeAdd from the write path",
			wantShared: []string{"KnowledgeAdd"},
		},
		{
			name:       "no overlap",
			a:          "internal/server.go:42 sets the retry ceiling",
			b:          "the weather is sunny and warm today",
			wantShared: nil,
		},
		{
			name:       "no referent in body → never matches",
			a:          "just some prose without any code anchor at all",
			b:          "also prose that happens to be nearby in the notes",
			wantShared: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sharedReferents(c.a, c.b)
			if strings.Join(got, ",") != strings.Join(c.wantShared, ",") {
				t.Errorf("sharedReferents = %v, want %v", got, c.wantShared)
			}
		})
	}
}

// TestKnowledgeAddWarnsReferentOverlap is the #167 Part 3 win case: two notes
// about the same file worded so differently that the Jaccard word-overlap check
// misses them, yet the referent-overlap scan surfaces the earlier note as a
// supersede candidate and names the shared file in the hint.
func TestKnowledgeAddWarnsReferentOverlap(t *testing.T) {
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

	// First note anchors on internal/server.go.
	first := add("internal/server.go:42 sets the retry ceiling; raise it when upstream flakes")
	if len(first.Similar) != 0 {
		t.Fatalf("first note should have no matches, got %v", first.Similar)
	}

	// Second note names the SAME file at a different line, with near-zero word
	// overlap — Jaccard would miss it; referent overlap must catch it.
	second := add("grpc deadline constant now lives at internal/server.go:88, not the http path")
	if len(second.Similar) == 0 {
		t.Fatal("expected a referent-overlap supersede candidate on the second add")
	}
	if !strings.Contains(second.Hint, "already speak to internal/server.go") {
		t.Errorf("hint should name the shared file, got %q", second.Hint)
	}
	if !strings.Contains(second.Similar[0].Body, "retry ceiling") {
		t.Errorf("surfaced note should be the first one, got %q", second.Similar[0].Body)
	}

	// A note about an unrelated file shares no referent → no supersede nudge.
	unrelated := add("cmd/dex/main.go:10 wires the dispatch table for subcommands")
	if len(unrelated.Similar) != 0 {
		t.Errorf("unrelated referent should not match, got %v", unrelated.Similar)
	}
}

// TestKnowledgeAddSkipsReferentNudgeWhenSuperseding verifies the write is not
// nagged to supersede when it is already superseding (#167 Part 3 gate).
func TestKnowledgeAddSkipsReferentNudgeWhenSuperseding(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, first, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "internal/server.go:42 sets the retry ceiling", Confidence: 0.8,
	})
	if err != nil || first.Status != "ok" {
		t.Fatalf("first add: status=%q err=%v", first.Status, err)
	}
	_, rec, err := rememberVerb(ctx, s, nil, RememberInput{ProjectRoot: root, Query: "retry ceiling", K: 5})
	if err != nil || len(rec.Result.Facts) == 0 {
		t.Fatalf("recall first: err=%v facts=%d", err, len(rec.Result.Facts))
	}
	firstID := rec.Result.Facts[0].ID

	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "grpc deadline lives at internal/server.go:88", Confidence: 0.8,
		SupersedesID: firstID,
	})
	if err != nil || out.Status != "ok" {
		t.Fatalf("supersede add: status=%q err=%v", out.Status, err)
	}
	if strings.Contains(out.Hint, "already speak to") {
		t.Errorf("superseding write should not be nagged to supersede, got hint %q", out.Hint)
	}
}
