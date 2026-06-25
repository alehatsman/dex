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
	"strings"
	"time"
)

// Project identifies an indexed project on disk.
type Project struct {
	Root     string // canonical absolute path
	ID       string // sha256(Root) hex — primary key for the cache dir
	CacheDir string // $DEX_INDEX_DIR/<ID>
	DBPath   string // CacheDir/index.db
	LockPath string // CacheDir/index.lock — cross-process indexer mutex
	// ActivityPath is a marker file whose mtime tracks the last
	// foreground query (search / ask / symbol / graph); MCP query handlers
	// stamp it via MarkActivity. Cross-process by construction (shared
	// filesystem) so any process can read the last-query time.
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
		Root:         real,
		ID:           id,
		CacheDir:     cache,
		DBPath:       filepath.Join(cache, "index.db"),
		LockPath:     filepath.Join(cache, "index.lock"),
		ActivityPath: filepath.Join(cache, "last_query"),
	}, nil
}

// ResolveDeleted computes the Project identity for a path that no longer
// exists on disk. Use only for cleanup (nuke) operations — the returned
// Root may not be canonical.
func ResolveDeleted(path, baseCacheDir string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path %q: %w", path, err)
	}
	// Resolve symlinks on the nearest surviving ancestor so the ID matches
	// what Resolve would have produced (e.g. macOS /var → /private/var).
	real := resolveNearestAncestor(abs)
	sum := sha256.Sum256([]byte(real))
	id := hex.EncodeToString(sum[:])
	cache := filepath.Join(baseCacheDir, id)
	return &Project{
		Root:         real,
		ID:           id,
		CacheDir:     cache,
		DBPath:       filepath.Join(cache, "index.db"),
		LockPath:     filepath.Join(cache, "index.lock"),
		ActivityPath: filepath.Join(cache, "last_query"),
	}, nil
}

// resolveNearestAncestor walks up path until it finds an existing ancestor,
// calls EvalSymlinks on it, and reattaches the deleted suffix. This makes
// ResolveDeleted produce IDs consistent with Resolve on platforms (like macOS)
// where temp-dir ancestors are symlinked (e.g. /var → /private/var).
func resolveNearestAncestor(path string) string {
	dir := path
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			suffix := strings.TrimPrefix(path, dir)
			return resolved + suffix
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return path
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
