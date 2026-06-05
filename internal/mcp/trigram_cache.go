package mcp

import (
	"sync"

	"github.com/alehatsman/dex/internal/trigram"
)

// trigramCacheKey identifies a set of files for a trigram index.
type trigramCacheKey struct {
	root   string
	prefix string
	ext    string
}

// trigramCacheEntry holds a built index and guards background rebuilds.
type trigramCacheEntry struct {
	mu      sync.Mutex
	idx     *trigram.Index
	building bool
}

// trigramCache stores per-(root,prefix,ext) trigram indices.
type trigramCache struct {
	m sync.Map // trigramCacheKey → *trigramCacheEntry
}

// getOrBuild returns a trigram index for the given key and file list.
// If a fresh index already exists it is returned immediately.
// If the index is stale, a background rebuild is triggered and the stale
// index is returned for this call (so we never block a search on a rebuild).
// The first call for a key builds synchronously.
func (c *trigramCache) getOrBuild(key trigramCacheKey, files []string) *trigram.Index {
	v, _ := c.m.LoadOrStore(key, &trigramCacheEntry{})
	entry := v.(*trigramCacheEntry)

	entry.mu.Lock()
	idx := entry.idx
	entry.mu.Unlock()

	if idx == nil {
		// First build — do it synchronously so the first call benefits.
		entry.mu.Lock()
		if entry.idx == nil {
			entry.idx = trigram.Build(files)
		}
		idx = entry.idx
		entry.mu.Unlock()
		return idx
	}

	if !idx.Stale() {
		return idx
	}

	// Stale — trigger background rebuild, return the stale index now.
	entry.mu.Lock()
	alreadyBuilding := entry.building
	if !alreadyBuilding {
		entry.building = true
	}
	entry.mu.Unlock()

	if !alreadyBuilding {
		go func() {
			newIdx := trigram.Build(files)
			entry.mu.Lock()
			entry.idx = newIdx
			entry.building = false
			entry.mu.Unlock()
		}()
	}

	return idx
}

// invalidate drops the cached index for a key, forcing a fresh build next call.
func (c *trigramCache) invalidate(key trigramCacheKey) {
	c.m.Delete(key)
}

