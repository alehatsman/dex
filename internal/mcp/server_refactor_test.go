package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRefactorUnsupportedOp(t *testing.T) {
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.refactor(context.Background(), nil, RefactorInput{
		Op: "extract_method", Symbol: "Foo", To: "Bar", ProjectRoot: t.TempDir(),
	})
	if out.Status != "error" || out.Hint == "" {
		t.Errorf("unsupported op = (%q,%q), want error with hint", out.Status, out.Hint)
	}
}

func TestRefactorMissingArgs(t *testing.T) {
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.refactor(context.Background(), nil, RefactorInput{
		Op: "rename_symbol", Symbol: "Foo", ProjectRoot: t.TempDir(), // no To
	})
	if out.Status != "error" {
		t.Errorf("missing `to` = %q, want error", out.Status)
	}
}

func TestRefactorRenameHappyPath(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	mod := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/rf\n\ngo 1.21\n")
	write("a.go", "package main\n\nfunc Greet() string { return \"hi\" }\n\nfunc main() { _ = Greet() }\n")

	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.refactor(context.Background(), nil, RefactorInput{
		Op: "rename_symbol", Symbol: "Greet", To: "Welcome", ProjectRoot: mod,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Op != "rename_symbol" || out.From != "Greet" || out.To != "Welcome" {
		t.Errorf("envelope wrong: %+v", out)
	}
	if len(out.Edits) != 2 { // def + call site
		t.Errorf("edits = %d, want 2: %+v", len(out.Edits), out.Edits)
	}
	if out.Etag == "" {
		t.Error("expected an etag")
	}
}

// TestRefactorInDefaultSurface guards that refactor ships in the everyday tool
// surface (not behind DEX_EXPERT) — it's a headline S-tier verb.
func TestRefactorInDefaultSurface(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	names := listToolNames(t, stubServer(t))
	if !names["refactor"] {
		t.Error("default surface omitted verb \"refactor\"; want it advertised")
	}
}
