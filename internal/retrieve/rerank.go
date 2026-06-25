package retrieve

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
)

// RerankFused is the single cross-encoder rerank entry point. It runs the
// cross-encoder over the candidate pool when a reranker is wired and the pool
// exceeds k — its ordering is authoritative and it populates Hit.RerankScore —
// and otherwise (or on a reranker outage) falls back to the canonical local
// quality rerank (store.ApplyLocalRerank). Trims to k. This guarantees the
// rerank runs exactly once over the complete candidate set.
//
// This is the ranking *policy* that used to live in the store (#473): the
// store is now a pure retrieval mechanism that delegates reranking to this
// service through the store.RerankFunc hook, and the mcp search tools call it
// directly over their own fused union.
func (svc Service) RerankFused(ctx context.Context, queryText string, hits []store.Hit, k int) ([]store.Hit, error) {
	if k <= 0 {
		k = 8
	}
	if svc.Rerank != nil && len(hits) > k {
		docs := make([]string, len(hits))
		for i := range hits {
			docs[i] = hits[i].Content
		}
		// In-process LRU keyed on (query, ordered docs). Interactive sessions
		// re-issue the same query repeatedly, and the cross-encoder call is the
		// most expensive leg — an identical (query, pool) returns the prior
		// scores without a second network call (#191).
		cacheKey := rerankDocsCacheKey(queryText, docs)
		var (
			scores []rerank.Score
			err    error
		)
		if svc.RerankCache != nil {
			if cached, ok := svc.RerankCache.Get(cacheKey); ok {
				scores = cached
			}
		}
		if scores == nil {
			scores, err = svc.rerankDocs(ctx, queryText, docs)
			if err == nil && svc.RerankCache != nil {
				svc.RerankCache.Put(cacheKey, scores)
			}
		}
		switch {
		case err == nil:
			ordered := make([]store.Hit, 0, len(scores))
			for _, sc := range scores {
				if sc.Index < 0 || sc.Index >= len(hits) {
					continue
				}
				h := hits[sc.Index]
				h.RerankScore = sc.Score
				ordered = append(ordered, h)
			}
			if len(ordered) > k {
				ordered = ordered[:k]
			}
			return ordered, nil
		case errors.Is(err, rerank.ErrUnreachable):
			// reranker outage — fall through to the local quality rerank
		default:
			return nil, err
		}
	}
	out := store.ApplyLocalRerank(hits, store.ClassifyQueryType(queryText) == store.QueryTypeSymbol, svc.DefinitionBoost)
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// rerankDocs delegates to the configured reranker under the per-call deadline
// (Service.RerankTimeout, default 1500ms) and maps a deadline expiry to
// rerank.ErrUnreachable so callers degrade to the pre-rerank ordering instead
// of surfacing a hard search failure. A caller-cancelled ctx is reported as-is.
func (svc Service) rerankDocs(ctx context.Context, queryText string, docs []string) ([]rerank.Score, error) {
	timeout := svc.RerankTimeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	rerankCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scores, err := svc.Rerank.Rerank(rerankCtx, queryText, docs)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: rerank timed out after %s", rerank.ErrUnreachable, timeout)
		}
		return nil, err
	}
	return scores, nil
}

// RerankCache memoizes cross-encoder scores across calls, keyed on
// (query, ordered docs) — see rerankDocsCacheKey. Safe for concurrent use.
// A single instance is shared process-wide (built once at wiring time) so the
// store rerank hook and the mcp search tools hit the same cache.
type RerankCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	index map[string]*list.Element
}

type rerankEntry struct {
	key    string
	scores []rerank.Score
}

// NewRerankCache returns a fixed-capacity LRU. capacity <= 0 defaults to 256 —
// plenty for an interactive session (~200 bytes/entry, well under 100 KiB).
func NewRerankCache(capacity int) *RerankCache {
	if capacity <= 0 {
		capacity = 256
	}
	return &RerankCache{
		cap:   capacity,
		ll:    list.New(),
		index: make(map[string]*list.Element, capacity),
	}
}

// Get returns the cached scores for key and promotes the entry to most-recent.
func (c *RerankCache) Get(key string) ([]rerank.Score, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*rerankEntry)
	return e.scores, true
}

// Put inserts or refreshes key, evicting the least-recently-used entry when
// the cache is over capacity.
func (c *RerankCache) Put(key string, scores []rerank.Score) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		el.Value.(*rerankEntry).scores = scores
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&rerankEntry{key: key, scores: scores})
	c.index[key] = el
	if c.ll.Len() > c.cap {
		if oldest := c.ll.Back(); oldest != nil {
			c.ll.Remove(oldest)
			delete(c.index, oldest.Value.(*rerankEntry).key)
		}
	}
}

// rerankDocsCacheKey builds a stable key from (query, ordered docs). Doc order
// is preserved and hashed in — rerank.Score.Index maps positionally onto the
// docs slice, so two pools that differ only in order are NOT interchangeable.
// Each doc is length-prefixed so adjacent docs can't be confused for one
// another ("ab","c" vs "a","bc").
func rerankDocsCacheKey(query string, docs []string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(query))
	_, _ = h.Write([]byte{0})
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(docs)))
	_, _ = h.Write(buf[:])
	for _, d := range docs {
		binary.LittleEndian.PutUint64(buf[:], uint64(len(d)))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(d))
	}
	return "d:" + strconv.FormatUint(h.Sum64(), 16)
}
