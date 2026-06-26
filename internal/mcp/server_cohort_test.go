package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCohortMissingArg(t *testing.T) {
	s := &Server{IndexDir: t.TempDir()}
	_, out, _ := s.cohort(context.Background(), nil, CohortInput{ProjectRoot: t.TempDir()})
	if out.Status != "error" {
		t.Errorf("missing interface = %q, want error", out.Status)
	}
}

func TestCohortHappyPath(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	mod := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/ch\n\ngo 1.21\n")
	write("a.go", `package main

type I interface{ M() }

type A struct{}

func (A) M() {}

type B struct{} // missing M

func main() {}
`)
	s := &Server{IndexDir: t.TempDir()}
	_, out, err := s.cohort(context.Background(), nil, CohortInput{Interface: "I", ProjectRoot: mod})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Complete != 1 {
		t.Errorf("complete = %d, want 1 (A)", out.Complete)
	}
}

// TestCohortIsExpertGated guards that cohort sits in the DEX_EXPERT power lane,
// not the everyday default surface (it's an occasional planning verb).
func TestCohortIsExpertGated(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	if listToolNames(t, stubServer(t))["cohort"] {
		t.Error("cohort should NOT be in the default surface (expected DEX_EXPERT-gated)")
	}
	t.Setenv("DEX_EXPERT", "1")
	if !listToolNames(t, stubServer(t))["cohort"] {
		t.Error("cohort should be advertised when DEX_EXPERT is set")
	}
}
