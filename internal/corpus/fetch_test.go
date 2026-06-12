package corpus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortSHA(t *testing.T) {
	if got := shortSHA(validSHA); got != "2efaec117637" {
		t.Errorf("shortSHA = %q, want 2efaec117637", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA short = %q, want abc", got)
	}
}

// TestEnsureLocal exercises the clone + pinned-checkout + cache-hit path against
// a local source repo (no network). It is skipped when git is unavailable or
// when the local git build rejects the blob-filtered clone — those are
// environment limitations, not harness bugs.
func TestEnsureLocal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	src := t.TempDir()
	runOrSkip(t, src, "init", "-q")
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOrSkip(t, src, "add", ".")
	// Pass identity inline so nothing is written to any git config file
	// (neither the temp repo's .git/config nor the user's global config).
	runOrSkip(t, src, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "feat: initial")
	sha, err := gitOutput(ctx, src, "rev-parse", "HEAD")
	if err != nil {
		t.Skipf("rev-parse: %v", err)
	}

	spec := RepoSpec{Name: "local", URL: src, Commit: sha}
	cacheRoot := t.TempDir()

	dir, err := Ensure(ctx, spec, cacheRoot)
	if err != nil {
		if strings.Contains(err.Error(), "filter") {
			t.Skipf("local git rejects blob-filtered clone: %v", err)
		}
		t.Fatalf("Ensure: %v", err)
	}
	if filepath.Base(dir) != "local@"+shortSHA(sha) {
		t.Errorf("cache dir = %q, want suffix local@%s", dir, shortSHA(sha))
	}
	head, err := gitHead(ctx, dir)
	if err != nil || head != sha {
		t.Fatalf("HEAD = %q (err %v), want %s", head, err, sha)
	}

	// Second call is a cache hit: same dir, no re-clone.
	dir2, err := Ensure(ctx, spec, cacheRoot)
	if err != nil || dir2 != dir {
		t.Fatalf("cache-hit Ensure = %q (err %v), want %s", dir2, err, dir)
	}
}

func runOrSkip(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := runGit(context.Background(), dir, args...); err != nil {
		t.Skipf("git %v: %v", args, err)
	}
}

// TestRunGitIgnoresInheritedGitDir guards issue #341: git injects GIT_DIR
// (pointing at the worktree gitdir) into pre-commit/pre-push hook children, so
// when the suite runs from a linked worktree the corpus tests inherit it. A
// bare `git init` honoring that GIT_DIR reinitializes the real repo as bare and
// flips the shared core.bare=true, wiping every worktree. runGit/gitOutput must
// scrub GIT_DIR so corpus git ops stay hermetic.
func TestRunGitIgnoresInheritedGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	// Sentinel repo with a linked worktree, giving us a worktree-style gitdir
	// (basename != ".git") — the shape that makes `git init` infer bare.
	sentinel := t.TempDir()
	setup := func(dir string, args ...string) {
		t.Helper()
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		c.Env = hermeticGitEnv() // build the sentinel free of any ambient GIT_DIR
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	setup(sentinel, "init", "-q")
	setup(sentinel, "-c", "user.email=t@example.com", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "init")
	setup(sentinel, "worktree", "add", "-q", filepath.Join(sentinel, "wt"))

	wtGitDir := filepath.Join(sentinel, ".git", "worktrees", "wt")
	if _, err := os.Stat(wtGitDir); err != nil {
		t.Fatalf("worktree gitdir missing: %v", err)
	}

	// Poison the environment exactly as the hook does.
	t.Setenv("GIT_DIR", wtGitDir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(wtGitDir, "index"))

	// runGit must target `work`, not the inherited GIT_DIR.
	work := t.TempDir()
	if err := runGit(ctx, work, "init", "-q"); err != nil {
		t.Fatalf("runGit init: %v", err)
	}

	// The sentinel's shared config must be untouched.
	out, err := gitOutput(ctx, sentinel, "config", "--get", "core.bare")
	if err != nil {
		out = "false" // `--get` exits 1 when absent — also "not bare"
	}
	if strings.TrimSpace(out) == "true" {
		t.Fatal("inherited GIT_DIR leaked: sentinel core.bare flipped to true")
	}
}
