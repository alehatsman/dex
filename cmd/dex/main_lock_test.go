package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/veccache"
)

// TestClearCacheKeepLockPreservesVecCache guards #121: the reindex sweep must
// keep the vector cache (and its WAL/SHM) alongside the lock and committed DB,
// so a reindex reuses vectors instead of re-embedding — while still removing
// stale cache artifacts.
func TestClearCacheKeepLockPreservesVecCache(t *testing.T) {
	dir := t.TempDir()
	p := &proj.Project{
		CacheDir: dir,
		LockPath: filepath.Join(dir, "lock"),
		DBPath:   filepath.Join(dir, "index.db"),
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lock")
	write("index.db")
	write(veccache.FileName)
	write(veccache.FileName + "-wal")
	write(veccache.FileName + "-shm")
	write("stale.tmp") // must be swept

	if err := clearCacheKeepLock(p); err != nil {
		t.Fatal(err)
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	for _, keep := range []string{
		"lock", "index.db",
		veccache.FileName, veccache.FileName + "-wal", veccache.FileName + "-shm",
	} {
		if !exists(keep) {
			t.Errorf("%s was swept but must be preserved", keep)
		}
	}
	if exists("stale.tmp") {
		t.Error("stale.tmp should have been swept")
	}
}
