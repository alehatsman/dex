package mcp

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
)

// TestGraphDiffRefValidation verifies that the ref allowlist rejects
// injection-style inputs before any git subprocess is spawned, and
// that well-formed refs pass through to the git layer.
func TestGraphDiffRefValidation(t *testing.T) {
	root := t.TempDir()
	indexDir := t.TempDir()

	// Resolve the project and create a stub DB file so the os.Stat
	// check passes. The file is empty — git will fail, but we only
	// care whether the ref is accepted or rejected here.
	p, err := proj.Resolve(root, indexDir)
	if err != nil {
		t.Fatalf("proj.Resolve: %v", err)
	}
	if err := os.MkdirAll(p.CacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	f, err := os.Create(p.DBPath)
	if err != nil {
		t.Fatalf("create stub db: %v", err)
	}
	f.Close()

	srv := &Server{IndexDir: indexDir}
	ctx := context.Background()

	// Refs that must be rejected with an "invalid ref" hint.
	badRefs := []string{
		"--upload-pack=evil", // flag injection via =
		"ref; rm -rf /",      // shell command separator
		"$(evil)",            // command substitution
		"`evil`",             // backtick substitution
		"ref && bad",         // shell AND
		"ref | bad",          // pipe
		"ref\nmore",          // newline
		"ref ref",            // space
	}
	for _, ref := range badRefs {
		_, out, err := srv.graphDiff(ctx, nil, DiffInput{ProjectRoot: root, Ref: ref})
		if err != nil {
			t.Fatalf("graphDiff(%q): unexpected error %v", ref, err)
		}
		if out.Status != "error" {
			t.Errorf("graphDiff(%q): status=%q, want \"error\"", ref, out.Status)
		}
		if !strings.Contains(out.Hint, "invalid ref") {
			t.Errorf("graphDiff(%q): hint=%q, want to contain \"invalid ref\"", ref, out.Hint)
		}
	}

	// Valid refs must not be rejected by the allowlist (git will fail
	// because root is not a real repo, but the hint will say "diff: …"
	// rather than "invalid ref").
	goodRefs := []string{
		"HEAD~1", "HEAD^", "main", "origin/main",
		"v1.0.0", "abc1234", "feature/foo", "tag@{0}",
	}
	for _, ref := range goodRefs {
		_, out, err := srv.graphDiff(ctx, nil, DiffInput{ProjectRoot: root, Ref: ref})
		if err != nil {
			t.Fatalf("graphDiff(%q): unexpected error %v", ref, err)
		}
		if out.Status == "error" && strings.Contains(out.Hint, "invalid ref") {
			t.Errorf("graphDiff(%q): valid ref rejected by allowlist: hint=%q", ref, out.Hint)
		}
	}
}
