package gitworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMainWorktreeLinked drives a real `git worktree add` and asserts the linked
// worktree resolves back to the main working tree.
func TestMainWorktreeLinked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	main := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = main
		// Keep the sub-git hermetic: a linked-worktree parent env would corrupt it.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root")

	wt := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-q", wt)

	got, ok := MainWorktree(wt)
	if !ok {
		t.Fatal("MainWorktree(worktree) ok = false, want true")
	}
	// git may canonicalize symlinks (e.g. /var -> /private/var on macOS); compare
	// via EvalSymlinks so the assertion is path-normalization agnostic.
	wantResolved, _ := filepath.EvalSymlinks(main)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Fatalf("MainWorktree = %q, want %q", gotResolved, wantResolved)
	}
}

// TestMainWorktreeMainCheckout — the main checkout has a `.git` directory, so it
// is not a linked worktree and must not inherit.
func TestMainWorktreeMainCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := MainWorktree(root); ok {
		t.Fatal("MainWorktree(main checkout) ok = true, want false")
	}
}

// TestMainWorktreeNotGit — a plain directory with no `.git` never inherits.
func TestMainWorktreeNotGit(t *testing.T) {
	if _, ok := MainWorktree(t.TempDir()); ok {
		t.Fatal("MainWorktree(plain dir) ok = true, want false")
	}
}

// TestMainWorktreeSubmodule — a submodule's `.git` file points at
// `<super>/.git/modules/<name>` (no `worktrees` segment) and must not inherit.
func TestMainWorktreeSubmodule(t *testing.T) {
	super := t.TempDir()
	modDir := filepath.Join(super, ".git", "modules", "sub")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(super, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: "+modDir+"\n")
	if _, ok := MainWorktree(sub); ok {
		t.Fatal("MainWorktree(submodule) ok = true, want false")
	}
}

// TestMainWorktreeSynthetic exercises the pure parse path (relative gitdir +
// commondir) without invoking git, so the resolution logic is covered even where
// git is absent.
func TestMainWorktreeSynthetic(t *testing.T) {
	mainRoot := t.TempDir()
	name := "feature"
	gitDir := filepath.Join(mainRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..\n")

	wt := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")

	got, ok := MainWorktree(wt)
	if !ok || got != mainRoot {
		t.Fatalf("MainWorktree = (%q, %v), want (%q, true)", got, ok, mainRoot)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
