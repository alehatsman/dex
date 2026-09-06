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

// TestCohortNotStandaloneTool locks the #852 invariant: cohort is not a
// standalone MCP tool in any mode — folded into query(kind=cohort) by the
// query-unification MCP re-justification (the spec's own classification
// concludes cohort "fold[s] in", unlike plan_rename/rehearse_patch's
// genuinely different edit-plan contract). The underlying h.cohort handler
// stays (dispatchMisc calls it directly); only the redundant tool door is gone.
func TestCohortNotStandaloneTool(t *testing.T) {
	for _, expert := range []string{"", "1"} {
		t.Setenv("DEX_EXPERT", expert)
		if listToolNames(t, stubServer(t))["cohort"] {
			t.Errorf("DEX_EXPERT=%q: cohort advertised as a standalone tool; want it reachable only via query(kind=cohort)", expert)
		}
	}
}
