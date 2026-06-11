// Package proj resolves a user-supplied project path to a canonical
// project root and a deterministic per-project cache directory.
package proj

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Project identifies an indexed project on disk.
type Project struct {
	Root     string // canonical absolute path
	ID       string // sha256(Root) hex — primary key for the cache dir
	CacheDir string // $DEX_INDEX_DIR/<ID>
	DBPath   string // CacheDir/index.db
	LockPath string // CacheDir/index.lock — cross-process indexer mutex
	// DrainLockPath is a second, independent flock guarding background
	// summary draining. Distinct from LockPath so a drain never blocks
	// (or is blocked by) an index run, while still ensuring only ONE
	// process drains a given project's pending_summaries at a time —
	// otherwise every `dex serve` / `dex mcp` watcher would redundantly
	// generate the same summaries and saturate the GPU.
	DrainLockPath string // CacheDir/summary.lock
	// ActivityPath is a marker file whose mtime tracks the last
	// foreground query (search / ask / symbol / graph). Cross-process by
	// construction (shared filesystem): query handlers touch it, the
	// summary drainer stats it and yields while it's fresh so background
	// work doesn't starve interactive latency.
	ActivityPath string // CacheDir/last_query
}

// Resolve canonicalizes path and returns the project identity. The path
// must exist and be a directory. Errors are phrased for direct display
// to the user (no Go-internals like "eval symlinks: lstat …").
func Resolve(path, baseCacheDir string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path %q: %w", path, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("path does not exist: %s: %w", abs, os.ErrNotExist)
		}
		return nil, fmt.Errorf("resolve %s: %w", abs, err)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", real, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", real)
	}
	sum := sha256.Sum256([]byte(real))
	id := hex.EncodeToString(sum[:])
	cache := filepath.Join(baseCacheDir, id)
	return &Project{
		Root:          real,
		ID:            id,
		CacheDir:      cache,
		DBPath:        filepath.Join(cache, "index.db"),
		LockPath:      filepath.Join(cache, "index.lock"),
		DrainLockPath: filepath.Join(cache, "summary.lock"),
		ActivityPath:  filepath.Join(cache, "last_query"),
	}, nil
}

// ResolveDeleted computes the Project identity for a path that no longer
// exists on disk. The cache ID is sha256(abs(path)), which matches what
// Resolve would have produced when the path was not a symlink. Use only
// for cleanup (nuke) operations — the returned Root may not be canonical.
func ResolveDeleted(path, baseCacheDir string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path %q: %w", path, err)
	}
	sum := sha256.Sum256([]byte(abs))
	id := hex.EncodeToString(sum[:])
	cache := filepath.Join(baseCacheDir, id)
	return &Project{
		Root:          abs,
		ID:            id,
		CacheDir:      cache,
		DBPath:        filepath.Join(cache, "index.db"),
		LockPath:      filepath.Join(cache, "index.lock"),
		DrainLockPath: filepath.Join(cache, "summary.lock"),
		ActivityPath:  filepath.Join(cache, "last_query"),
	}, nil
}

// EnsureCacheDir creates the per-project cache directory.
func (p *Project) EnsureCacheDir() error {
	return os.MkdirAll(p.CacheDir, 0o755)
}

// MarkActivity stamps ActivityPath's mtime to now, recording that a
// foreground query just ran. Cheap (a touch); callers may throttle.
// Best-effort: a failure here must never break a query, so callers
// typically ignore the error.
func (p *Project) MarkActivity() error {
	if p.ActivityPath == "" {
		return nil
	}
	now := time.Now()
	err := os.Chtimes(p.ActivityPath, now, now)
	if errors.Is(err, os.ErrNotExist) {
		f, e := os.OpenFile(p.ActivityPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if e != nil {
			return e
		}
		_ = f.Close()
		return os.Chtimes(p.ActivityPath, now, now)
	}
	return err
}

// LastActivity returns the time of the last foreground query (the
// ActivityPath mtime) and whether the marker exists. A missing marker
// means "no recent activity" — callers treat ok=false as "go ahead".
func (p *Project) LastActivity() (time.Time, bool) {
	if p.ActivityPath == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(p.ActivityPath)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}
