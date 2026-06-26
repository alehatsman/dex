package mcp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSummarizeRefTimeTravel covers #657: the MCP read tool reads a file as of
// a git ref (full + signatures), not the working tree.
func TestSummarizeRefTimeTravel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	gitRun(t, projDir, "init", "-q")
	writeFile(t, filepath.Join(projDir, "calc.go"), "package calc\nfunc Add() int { return 1 }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v1")
	// v2 adds Sub and documents Add.
	writeFile(t, filepath.Join(projDir, "calc.go"),
		"package calc\n// Add returns one.\nfunc Add() int { return 1 }\nfunc Sub() int { return -1 }\n")
	gitRun(t, projDir, "add", ".")
	gitRun(t, projDir, "commit", "-q", "-m", "v2")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	// full @ HEAD~1 → the v1 version (no Sub, no doc).
	_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "calc.go", ProjectRoot: root, Mode: "full", Ref: "HEAD~1"})
	if err != nil || out.Status != "ok" {
		t.Fatalf("ref full: status=%q hint=%q err=%v", out.Status, out.Hint, err)
	}
	if !strings.Contains(out.Content, "Add") || strings.Contains(out.Content, "Sub") {
		t.Errorf("HEAD~1 content should be the v1 version (Add only):\n%s", out.Content)
	}

	// Working tree (no ref) → v2 (has Sub).
	_, cur, _ := s.summarize(ctx, nil, SummarizeInput{Path: "calc.go", ProjectRoot: root, Mode: "full"})
	if !strings.Contains(cur.Content, "Sub") {
		t.Errorf("working-tree read should be v2 (has Sub):\n%s", cur.Content)
	}

	// signatures @ HEAD~1 compresses the historical content (not the HEAD index).
	_, sig, _ := s.summarize(ctx, nil, SummarizeInput{Path: "calc.go", ProjectRoot: root, Mode: "signatures", Ref: "HEAD~1"})
	if sig.Status != "ok" || strings.Contains(sig.Content, "Sub") {
		t.Errorf("signatures @ HEAD~1 must reflect v1 (no Sub): status=%q\n%s", sig.Status, sig.Content)
	}

	// Index-backed mode with --ref → clear error.
	_, sk, _ := s.summarize(ctx, nil, SummarizeInput{Path: "calc.go", ProjectRoot: root, Mode: "skeleton", Ref: "HEAD~1"})
	if sk.Status != "error" || !strings.Contains(sk.Hint, "use full or signatures") {
		t.Errorf("skeleton+ref should error with guidance, got status=%q hint=%q", sk.Status, sk.Hint)
	}
}
