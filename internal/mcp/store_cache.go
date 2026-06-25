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
				st.SetGitRecency(gitrecency.New(root))
			}
		}
		cs.st, cs.err = st, err
	})
	return cs.st, cs.err
}
