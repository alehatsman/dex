package mcp

import (
	"context"
	"os"
	"sync"

	"github.com/alehatsman/dex/internal/gitrecency"
	"github.com/alehatsman/dex/internal/store"
)

// cachedStore holds a lazily-opened *store.Store for one project DB path.
// The store is opened once and reused across requests; never closed per-request.
// SQLite WAL mode supports concurrent readers on a single connection.
type cachedStore struct {
	once   sync.Once
	st     *store.Store
	gc     *gitrecency.Cache // same instance wired into st via SetGitRecency
	err    error
	dbStat os.FileInfo // inode/mtime at open time; used to detect db replacement
}

// openStore returns a cached *store.Store for dbPath, opening it once on
// first call. Subsequent calls return the cached connection without any
// file or migration overhead. If the db file was deleted and recreated at the
// same path (e.g. dex reindex), the stale fd is evicted and reopened (#753).
func (s *Server) openStore(dbPath string) (*store.Store, error) {
	for {
		v, _ := s.storeByPath.LoadOrStore(dbPath, &cachedStore{})
		cs, ok := v.(*cachedStore)
		if !ok {
			return store.OpenWith(context.Background(), dbPath, s.StoreOpts)
		}
		cs.once.Do(func() {
			st, err := store.OpenWith(context.Background(), dbPath, s.StoreOpts)
			if err == nil {
				cs.dbStat, _ = os.Stat(dbPath)
				if root, rerr := st.ProjectRoot(context.Background()); rerr == nil && root != "" {
					gc := gitrecency.New(root)
					st.SetGitRecency(gc)
					cs.gc = gc
				}
			}
			cs.st, cs.err = st, err
		})
		// Detect db replacement: if the inode at dbPath changed, the index was
		// rebuilt and the cached fd points at the old (now-unlinked) database.
		if cs.err == nil && cs.dbStat != nil {
			if cur, err := os.Stat(dbPath); err == nil && !os.SameFile(cs.dbStat, cur) {
				s.storeByPath.CompareAndDelete(dbPath, cs)
				continue // retry with a fresh cachedStore
			}
		}
		return cs.st, cs.err
	}
}

// storeGitRecency returns the TTL-cached *gitrecency.Cache that was wired into
// the store at open time, or nil if the store hasn't been opened yet or has no
// project root. taskMap uses this to avoid spawning a fresh git log per call.
func (s *Server) storeGitRecency(dbPath string) *gitrecency.Cache {
	v, ok := s.storeByPath.Load(dbPath)
	if !ok {
		return nil
	}
	cs, ok := v.(*cachedStore)
	if !ok {
		return nil
	}
	return cs.gc
}
