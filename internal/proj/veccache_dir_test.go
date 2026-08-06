package proj

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVecCacheDirSharedAcrossWorktree — a linked git worktree must resolve its
// vector cache to the main checkout's cache dir so every worktree of a repo
// shares one veccache.db (#123), while index.db stays per-project.
func TestVecCacheDirSharedAcrossWorktree(t *testing.T) {
	cache := t.TempDir()

	// Synthetic linked worktree: `.git` is a file pointing at
	// <mainRoot>/.git/worktrees/<name>, whose commondir walks back to mainRoot.
	mainRoot := t.TempDir()
	name := "feature"
	gitDir := filepath.Join(mainRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mainProj, err := Resolve(mainRoot, cache)
	if err != nil {
		t.Fatal(err)
	}
	wtProj, err := Resolve(wt, cache)
	if err != nil {
		t.Fatal(err)
	}

	// Distinct roots → distinct projects → distinct index.db.
	if wtProj.CacheDir == mainProj.CacheDir {
		t.Fatalf("worktree and main share a CacheDir %q; expected distinct per-project dirs", wtProj.CacheDir)
	}

	// But the vector cache is shared: the worktree points at main's cache dir.
	if got := wtProj.VecCacheDir(); got != mainProj.CacheDir {
		t.Fatalf("worktree VecCacheDir = %q, want main CacheDir %q", got, mainProj.CacheDir)
	}
}

// TestVecCacheDirMainCheckout — a normal checkout (`.git` dir) is not a linked
// worktree, so its vector cache stays in its own CacheDir.
func TestVecCacheDirMainCheckout(t *testing.T) {
	cache := t.TempDir()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Resolve(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.VecCacheDir(); got != p.CacheDir {
		t.Fatalf("main checkout VecCacheDir = %q, want own CacheDir %q", got, p.CacheDir)
	}
}

// TestVecCacheDirPlainDir — a non-git directory always uses its own CacheDir.
func TestVecCacheDirPlainDir(t *testing.T) {
	cache := t.TempDir()
	p, err := Resolve(t.TempDir(), cache)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.VecCacheDir(); got != p.CacheDir {
		t.Fatalf("plain dir VecCacheDir = %q, want own CacheDir %q", got, p.CacheDir)
	}
}
