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
	runOrSkip(t, src, "config", "user.email", "t@example.com")
	runOrSkip(t, src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOrSkip(t, src, "add", ".")
	runOrSkip(t, src, "commit", "-q", "-m", "feat: initial")
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
