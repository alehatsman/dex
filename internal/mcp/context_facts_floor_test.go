package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// TestCapFactBody covers the per-fact injection cap (#785): bodies over the
// limit are clipped on a rune boundary with a visible marker; short bodies pass
// through untouched.
func TestCapFactBody(t *testing.T) {
	short := "config is parsed from yaml not toml"
	if got := capFactBody(short); got != short {
		t.Errorf("short body altered: %q", got)
	}

	long := strings.Repeat("a", maxInjectedFactBody+200)
	got := capFactBody(long)
	if !strings.HasSuffix(got, " …(truncated)") {
		t.Errorf("over-long body missing marker: ...%q", got[len(got)-20:])
	}
	if n := len([]rune(strings.TrimSuffix(got, " …(truncated)"))); n > maxInjectedFactBody {
		t.Errorf("clipped body = %d runes, want <= %d", n, maxInjectedFactBody)
	}

	// Multibyte boundary: clipping must not split a rune.
	multi := strings.Repeat("é", maxInjectedFactBody+10)
	if !utf8ValidString(capFactBody(multi)) {
		t.Error("clipped multibyte body is not valid UTF-8")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestAskInjectsRelevantFactCapped proves the relevance floor does NOT block a
// fact that actually matches the question, and that an over-long matching body
// is clipped before injection (#785). The fake embedder is deterministic, so a
// question equal to a fact's body yields cosine 1.0 — comfortably above the 0.5
// floor.
func TestAskInjectsRelevantFactCapped(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	body := "the greeter " + strings.Repeat("greets users warmly ", 50) // > maxInjectedFactBody
	if _, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Architecture", Body: body, Confidence: 0.9,
	}); err != nil || out.Status != "ok" {
		t.Fatalf("add: status=%q err=%v", out.Status, err)
	}

	_, out, err := s.ContextRouter(ctx, ContextInput{Question: body, ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.KnowledgeFacts) != 1 {
		t.Fatalf("want 1 injected fact (matches the question), got %d: %v", len(out.KnowledgeFacts), out.KnowledgeFacts)
	}
	if !strings.HasSuffix(out.KnowledgeFacts[0], " …(truncated)") {
		t.Errorf("injected fact not capped: %q", out.KnowledgeFacts[0])
	}
}

// TestAskNoSalienceFallbackForUnmatchedFact pins the core behavior change: ask
// now recalls facts with skipFallback=true, so a high-salience fact that does
// NOT match the question is no longer injected via the salience fallback. A
// fact written straight to the store has no embedding, so the strict (floored)
// vector query returns nothing rather than falling back to top-salience — which
// is exactly how an off-topic, high-weight VerifiedFact used to leak in (#785).
func TestAskNoSalienceFallbackForUnmatchedFact(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()

	// Add a high-salience fact with no embedding by writing directly to the store.
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeAdd(ctx, "VerifiedFact",
		"QA iteration 999 — unrelated session log dump about indexing internals", 0.95); err != nil {
		t.Fatal(err)
	}
	st.Close()

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(ctx, ContextInput{Question: "where do we greet users", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.KnowledgeFacts) != 0 {
		t.Errorf("salience fallback leaked an unmatched fact into ask: %v", out.KnowledgeFacts)
	}
}
