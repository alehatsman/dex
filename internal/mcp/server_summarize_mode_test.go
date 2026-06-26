package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidReadMode(t *testing.T) {
	valid := []string{
		"full", "signatures", "skeleton", "map", "aggressive", "summary",
		"lines:1-10", "lines:bad", // lines:* parsing is validated downstream
		"handle",
	}
	for _, m := range valid {
		if !ValidReadMode(ReadMode(m)) {
			t.Errorf("ValidReadMode(%q) = false, want true", m)
		}
	}
	invalid := []string{
		"entropy", // CLI-only convenience, not a server mode
		"auto",    // CLI-only convenience
		"lines",   // bare stand-in, not dispatchable; needs lines:N-M
		"",        // empty is only valid as an *implicit* default, not explicit
		"xyzzy",
		"Full", // case-sensitive: resolve lowercases before this check
	}
	for _, m := range invalid {
		if ValidReadMode(ReadMode(m)) {
			t.Errorf("ValidReadMode(%q) = true, want false", m)
		}
	}
}

// TestSummarizeRejectsUnknownMode locks #528: an explicitly-requested mode the
// dispatch can't handle must error rather than silently serving the full raw
// file (a token blow-up).
func TestSummarizeRejectsUnknownMode(t *testing.T) {
	root := t.TempDir()
	file := "big.go"
	body := "package x\n" + strings.Repeat("// filler line\n", 500)
	if err := os.WriteFile(filepath.Join(root, file), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s := &Server{}
	ctx := context.Background()

	out, err := s.Summarize(ctx, SummarizeInput{Path: file, ProjectRoot: root, Mode: "entropy"})
	if err != nil {
		t.Fatalf("Summarize returned a transport error: %v", err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want \"error\" for unrecognized mode", out.Status)
	}
	if !strings.Contains(out.Hint, "unrecognized read mode") {
		t.Errorf("hint = %q, want it to flag the unrecognized mode", out.Hint)
	}
	// The raw file content must NOT leak through on the error path.
	if strings.Contains(out.Content, "filler line") {
		t.Errorf("error path leaked full file content (token blow-up regression)")
	}
}

// TestSummarizeSurfacesScopedNotes covers #650: reading a file surfaces notes
// whose scope binds its path, tagged with the matching scope.
func TestSummarizeSurfacesScopedNotes(t *testing.T) {
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	if _, _, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "greet.go: prefix needs a trailing space", Scope: "greet.go",
	}); err != nil {
		t.Fatal(err)
	}

	// Any read mode surfaces it — exercise full (raw, no index leg of its own).
	_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "greet.go", ProjectRoot: root, Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.ScopedNotes) != 1 {
		t.Fatalf("ScopedNotes = %d, want 1: %+v", len(out.ScopedNotes), out.ScopedNotes)
	}
	if out.ScopedNotes[0].Scope != "greet.go" || !strings.Contains(out.ScopedNotes[0].Body, "trailing space") {
		t.Errorf("wrong scoped note: %+v", out.ScopedNotes[0])
	}

	// A different file must not surface greet.go's note.
	writeFile(t, filepath.Join(projDir, "other.go"), "package main\n\nfunc Other() {}\n")
	if _, o2, _ := s.summarize(ctx, nil, SummarizeInput{Path: "other.go", ProjectRoot: root, Mode: "full"}); len(o2.ScopedNotes) != 0 {
		t.Errorf("other.go should surface no scoped notes, got %+v", o2.ScopedNotes)
	}
}
