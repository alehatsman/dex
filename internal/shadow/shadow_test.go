package shadow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// userGit runs a git command in the user's repo with a CLEAN environment (no
// inherited GIT_* and no shadow vars), so test assertions about the user repo
// are never confused by the shadow's env.
func userGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Strip every GIT_* var so this observes ONLY the repo at dir.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("user git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newUserRepo creates a real git repo with one commit and returns its dir.
func newUserRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	userGit(t, dir, "init", "--quiet")
	userGit(t, dir, "config", "user.email", "user@example.com")
	userGit(t, dir, "config", "user.name", "Real User")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userGit(t, dir, "add", "-A")
	userGit(t, dir, "commit", "--quiet", "-m", "initial")
	return dir
}

func TestShadowSnapshotIsolation(t *testing.T) {
	root := newUserRepo(t)
	cacheDir := t.TempDir()
	userHEADBefore := userGit(t, root, "rev-parse", "HEAD")

	r := Open(cacheDir, root)
	ctx := context.Background()

	// First snapshot stages the whole tree.
	res, err := r.Snapshot(ctx, "first checkpoint")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !res.Created || res.SHA == "" || res.FilesChanged == 0 {
		t.Fatalf("expected a real snapshot, got %+v", res)
	}

	// THE critical guarantee: the user's repo HEAD must not have moved, and the
	// shadow must live under cacheDir, never in the user's tree.
	if after := userGit(t, root, "rev-parse", "HEAD"); after != userHEADBefore {
		t.Fatalf("user repo HEAD moved! before=%s after=%s — isolation breached", userHEADBefore, after)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "shadow", "HEAD")); err != nil {
		t.Errorf("shadow repo not at cacheDir/shadow: %v", err)
	}
	// The user's working tree must be clean — the shadow's `add -A` must not have
	// staged anything in the USER index.
	if st := userGit(t, root, "status", "--porcelain"); st != "" {
		t.Errorf("user repo working tree dirtied by shadow: %q", st)
	}

	// Idempotency: no changes → no new commit.
	res2, err := r.Snapshot(ctx, "noop")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.SHA != res.SHA || res2.FilesChanged != 0 {
		t.Errorf("unchanged tree should not create a commit, got %+v", res2)
	}

	// A real change produces a new checkpoint.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res3, err := r.Snapshot(ctx, "added b")
	if err != nil {
		t.Fatal(err)
	}
	if !res3.Created || res3.SHA == res.SHA {
		t.Fatalf("expected a new checkpoint after a change, got %+v", res3)
	}

	// Log: newest first, both checkpoints present.
	commits, err := r.Log(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 checkpoints, got %d", len(commits))
	}
	if commits[0].Message != "added b" || commits[1].Message != "first checkpoint" {
		t.Errorf("log order wrong: %+v", commits)
	}

	// Diff HEAD~1..HEAD shows the added file.
	diff, err := r.Diff(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "b.txt") || !strings.Contains(diff, "+world") {
		t.Errorf("diff missing the b.txt change:\n%s", diff)
	}

	// User HEAD STILL unmoved after all operations.
	if after := userGit(t, root, "rev-parse", "HEAD"); after != userHEADBefore {
		t.Fatalf("user repo HEAD moved after full sequence: %s", after)
	}
}

// TestShadowResistsInjectedGitDir is the #341-hardening test: even when the
// process inherits GIT_DIR / GIT_WORK_TREE pointing at the user's repo (as a
// git hook injects), the shadow must scrub them and never commit to the user's
// repo.
func TestShadowResistsInjectedGitDir(t *testing.T) {
	root := newUserRepo(t)
	cacheDir := t.TempDir()
	userHEADBefore := userGit(t, root, "rev-parse", "HEAD")

	// Simulate the hazard: the dex process runs with GIT_DIR/GIT_WORK_TREE
	// pointing at the USER repo.
	t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
	t.Setenv("GIT_WORK_TREE", root)

	r := Open(cacheDir, root)
	if _, err := r.Snapshot(context.Background(), "hostile env"); err != nil {
		t.Fatalf("snapshot under injected env: %v", err)
	}

	if after := userGit(t, root, "rev-parse", "HEAD"); after != userHEADBefore {
		t.Fatalf("INJECTED GIT_DIR breached isolation — user HEAD moved to %s", after)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "shadow", "HEAD")); err != nil {
		t.Errorf("shadow not created under cacheDir despite injected env: %v", err)
	}
}

func TestShadowLogEmptyBeforeFirstSnapshot(t *testing.T) {
	root := newUserRepo(t)
	r := Open(t.TempDir(), root)
	commits, err := r.Log(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 0 {
		t.Errorf("expected no checkpoints before first snapshot, got %d", len(commits))
	}
	// Diff on an uninitialised shadow is empty, not an error.
	if diff, err := r.Diff(context.Background(), "", ""); err != nil || diff != "" {
		t.Errorf("uninitialised diff = %q, %v; want empty, nil", diff, err)
	}
}

func TestDiffRejectsBadRef(t *testing.T) {
	root := newUserRepo(t)
	r := Open(t.TempDir(), root)
	if _, err := r.Snapshot(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Diff(context.Background(), "HEAD; rm -rf /", "HEAD"); err == nil {
		t.Error("expected rejection of a ref with shell metacharacters")
	}
}
