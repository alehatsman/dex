package mcp

import (
	"context"
	"sync"

	"github.com/alehatsman/dex/internal/store"
)

// cachedMultiScale holds the last BuildMultiScale result and the
// last_indexed_at epoch at the time it was built.
type cachedMultiScale struct {
	mu    sync.Mutex
	stamp int64 // Stats().LastIndex.UnixNano()
	idx   *store.MultiScaleIndex
}

// cachedBuildMultiScale returns a MultiScaleIndex for the given store,
// rebuilding only when the index's last_indexed_at epoch has advanced.
// The cache entry is keyed by dbPath and stored on the Server.
func (s *Server) cachedBuildMultiScale(ctx context.Context, st *store.Store, dbPath string) (*store.MultiScaleIndex, error) {
	stats, err := st.Stats(ctx)
	if err != nil {
		return nil, err
	}
	stamp := stats.LastIndex.UnixNano()

	v, _ := s.multiScaleByPath.LoadOrStore(dbPath, &cachedMultiScale{})
	cm, ok := v.(*cachedMultiScale)
	if !ok {
		return st.BuildMultiScale(ctx)
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.idx != nil && cm.stamp == stamp {
		return cm.idx, nil
	}

	idx, err := st.BuildMultiScale(ctx)
	if err != nil {
		return nil, err
	}
	cm.idx = idx
	cm.stamp = stamp
	return idx, nil
}
