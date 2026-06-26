package mcp

import (
	"context"
	"sync"

	"github.com/alehatsman/dex/internal/gitrecency"
	"github.com/alehatsman/dex/internal/store"
)

// cachedStore holds a lazily-opened *store.Store for one project DB path.
// The store is opened once and reused across requests; never closed per-request.
// SQLite WAL mode supports concurrent readers on a single connection.
type cachedStore struct {
	once sync.Once
	st   *store.Store
	gc   *gitrecency.Cache // same instance wired into st via SetGitRecency
	err  error
}

// openStore returns a cached *store.Store for dbPath, opening it once on
// first call. Subsequent calls return the cached connection without any
// file or migration overhead.
func (s *Server) openStore(dbPath string) (*store.Store, error) {
	v, _ := s.storeByPath.LoadOrStore(dbPath, &cachedStore{})
	cs, ok := v.(*cachedStore)
	if !ok {
		return store.OpenWith(context.Background(), dbPath, s.StoreOpts)
	}
	cs.once.Do(func() {
		st, err := store.OpenWith(context.Background(), dbPath, s.StoreOpts)
		if err == nil {
			if root, rerr := st.ProjectRoot(context.Background()); rerr == nil && root != "" {
				gc := gitrecency.New(root)
				st.SetGitRecency(gc)
				cs.gc = gc
			}
		}
		cs.st, cs.err = st, err
	})
	return cs.st, cs.err
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
