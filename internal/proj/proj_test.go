package proj

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBasics(t *testing.T) {
	cache := t.TempDir()
	dir := t.TempDir()
	p, err := Resolve(dir, cache)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root == "" || p.ID == "" || p.CacheDir == "" || p.DBPath == "" || p.LockPath == "" {
		t.Errorf("Project missing fields: %+v", p)
	}
	if filepath.Dir(p.DBPath) != p.CacheDir {
		t.Errorf("DBPath %q not under CacheDir %q", p.DBPath, p.CacheDir)
	}
	if filepath.Dir(p.LockPath) != p.CacheDir {
		t.Errorf("LockPath %q not under CacheDir %q", p.LockPath, p.CacheDir)
	}
	if len(p.ID) != 64 {
		t.Errorf("ID should be sha256 hex (64 chars); got %d", len(p.ID))
	}
}

func TestResolveDeterministic(t *testing.T) {
	cache := t.TempDir()
	dir := t.TempDir()
	a, _ := Resolve(dir, cache)
	b, _ := Resolve(dir, cache)
	if a.ID != b.ID {
		t.Errorf("Resolve not deterministic: %s vs %s", a.ID, b.ID)
	}
}

func TestResolveDifferentRootsDifferentIDs(t *testing.T) {
	cache := t.TempDir()
	a, _ := Resolve(t.TempDir(), cache)
	b, _ := Resolve(t.TempDir(), cache)
	if a.ID == b.ID {
		t.Error("different roots produced the same ID")
	}
}

func TestResolveRelativePath(t *testing.T) {
	cache := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	parent := t.TempDir()
	_ = os.Mkdir(filepath.Join(parent, "sub"), 0o755)
	_ = os.Chdir(parent)
	p, err := Resolve("sub", cache)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Root) {
		t.Errorf("Root not absolute: %q", p.Root)
	}
}

func TestResolveErrors(t *testing.T) {
	cache := t.TempDir()
	_, err := Resolve("/this/path/should/not/exist/anywhere", cache)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected friendly error, got %q", err)
	}

	// Pointing at a regular file rather than a dir.
	f := filepath.Join(t.TempDir(), "file.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	_, err = Resolve(f, cache)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' error, got %v", err)
	}
}

func TestResolveWrapsErrNotExist(t *testing.T) {
	cache := t.TempDir()
	_, err := Resolve("/this/path/does/not/exist", cache)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error to wrap os.ErrNotExist, got %v", err)
	}
}

func TestResolveDeleted(t *testing.T) {
	cache := t.TempDir()
	dir := t.TempDir()

	// Resolve while dir exists to get the canonical ID.
	live, err := Resolve(dir, cache)
	if err != nil {
		t.Fatal(err)
	}

	// Remove the directory and resolve via ResolveDeleted.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	dead, err := ResolveDeleted(dir, cache)
	if err != nil {
		t.Fatal(err)
	}

	// IDs must match so we can find and delete the existing cache.
	if live.ID != dead.ID {
		t.Errorf("ID mismatch: live=%s dead=%s", live.ID, dead.ID)
	}
	if dead.CacheDir != live.CacheDir {
		t.Errorf("CacheDir mismatch: live=%s dead=%s", live.CacheDir, dead.CacheDir)
	}
}

func TestEnsureCacheDir(t *testing.T) {
	cache := t.TempDir()
	dir := t.TempDir()
	p, _ := Resolve(dir, cache)
	if _, err := os.Stat(p.CacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CacheDir should not exist before EnsureCacheDir: %v", err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.CacheDir); err != nil {
		t.Errorf("CacheDir not created: %v", err)
	}
}
