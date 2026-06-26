package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testGitEnv strips every GIT_* var from environ — so an inherited GIT_DIR
// (the dex pre-push hook exports one whose basename ≠ ".git") can't redirect a
// git command at the shared repo and flip its core.bare (#681) — and pins
// identity + config to fixed, developer-independent values. Pure so it can be
// unit-tested without spawning git or mutating the process env.
func testGitEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+6)
	for _, kv := range environ {
		if strings.HasPrefix(kv, "GIT_") {
			continue // drop GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE/etc.
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// git runs a git command in dir with the hermetic testGitEnv plus
// core.hooksPath=/dev/null, so a commit in the temp repo never fires the dex
// pre-commit hook (#679). Mirrors the sibling helpers (internal/mcp gitRun,
// internal/gitrecency gitEnv).
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = testGitEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitCommit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", file)
	git(t, dir, "commit", "-qm", msg)
}

func TestReadAtRefTimeTravel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	gitCommit(t, dir, "f.go", "package p\nfunc V1() {}\n", "v1")
	gitCommit(t, dir, "f.go", "package p\nfunc V2() {}\n", "v2")

	ctx := context.Background()
	path := filepath.Join(dir, "f.go")

	old, err := ReadAtRef(ctx, path, "HEAD~1")
	if err != nil {
		t.Fatalf("ReadAtRef HEAD~1: %v", err)
	}
	if !strings.Contains(string(old), "V1") || strings.Contains(string(old), "V2") {
		t.Errorf("HEAD~1 = %q, want the V1 version", old)
	}
	cur, err := ReadAtRef(ctx, path, "HEAD")
	if err != nil {
		t.Fatalf("ReadAtRef HEAD: %v", err)
	}
	if !strings.Contains(string(cur), "V2") {
		t.Errorf("HEAD = %q, want the V2 version", cur)
	}
}

// TestHermeticGitEnvDropsInheritedDirs locks #681/#679: the git() helper must
// never pass an inherited GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE to the child git.
// Leaking a worktree-style GIT_DIR lets a `git init`/commit in a temp repo flip
// the shared checkout's core.bare (the recurring bare-repo / index-wipe hazard).
func TestHermeticGitEnvDropsInheritedDirs(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/repo/.git/worktrees/wt", // the dangerous inherited shape
		"GIT_WORK_TREE=/repo",
		"GIT_INDEX_FILE=/repo/.git/worktrees/wt/index",
		"HOME=/home/u",
	}
	out := testGitEnv(in)
	for _, kv := range out {
		for _, banned := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE="} {
			if strings.HasPrefix(kv, banned) {
				t.Errorf("testGitEnv leaked %q — an inherited GIT_DIR can flip a shared repo's core.bare (#681)", kv)
			}
		}
	}
	// Non-GIT vars survive and identity is pinned.
	want := map[string]bool{"PATH=/usr/bin": false, "HOME=/home/u": false, "GIT_AUTHOR_NAME=t": false}
	for _, kv := range out {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for kv, seen := range want {
		if !seen {
			t.Errorf("testGitEnv dropped expected entry %q", kv)
		}
	}
}

func TestReadAtRefRejectsOptionRef(t *testing.T) {
	_, err := ReadAtRef(context.Background(), "f.go", "-x")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("option-like ref must be rejected, got err=%v", err)
	}
}

func TestReadAtRefBadRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	gitCommit(t, dir, "f.go", "package p\n", "v1")
	if _, err := ReadAtRef(context.Background(), filepath.Join(dir, "f.go"), "nonexistent-ref"); err == nil {
		t.Fatal("expected an error for a nonexistent ref")
	}
}
