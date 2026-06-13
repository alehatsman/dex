package mcp

import (
	"context"
	"sync"

	"github.com/alehatsman/dex/internal/store"
)

// cachedGraphView holds a graphView and the graph epoch (max last_seen_at) at
// the time it was loaded. A cache miss occurs only when the epoch advances.
type cachedGraphView struct {
	mu    sync.Mutex
	epoch int64
	view  *graphView
}

// cachedLoadGraphView returns a graphView for the given store, loading it
// fresh only when the graph epoch has advanced since the last load. The
// cache entry is keyed by dbPath and stored on the Server.
func (s *Server) cachedLoadGraphView(ctx context.Context, st *store.Store, dbPath string) (*graphView, error) {
	epoch, err := st.GraphMaxEpoch(ctx)
	if err != nil {
		return nil, err
	}

	v, _ := s.graphViewByPath.LoadOrStore(dbPath, &cachedGraphView{})
	cv := v.(*cachedGraphView)

	cv.mu.Lock()
	defer cv.mu.Unlock()

	if cv.view != nil && cv.epoch == epoch {
		return cv.view, nil
	}

	view, err := loadGraphView(ctx, st)
	if err != nil {
		return nil, err
	}
	cv.view = view
	cv.epoch = epoch
	return view, nil
}
