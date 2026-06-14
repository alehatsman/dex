package mcp

import (
	"hash/fnv"
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
	mu       sync.Mutex
	idx      *trigram.Index
	filesFP  uint64 // fingerprint of the file SET idx was built from
	building bool
}

// filesFingerprint is an order-independent fingerprint of a file set. It lets
// the cache detect when the *membership* of the candidate list has changed —
// not just the mtime/size of files already indexed, which is all
// trigram.Index.Stale() can observe. The XOR fold is order-independent (the
// store may return paths in a different order across calls); the count is
// mixed in so an add/remove pair that happens to XOR-cancel still differs.
func filesFingerprint(files []string) uint64 {
	var fp uint64
	for _, f := range files {
		h := fnv.New64a()
		_, _ = h.Write([]byte(f))
		fp ^= h.Sum64()
	}
	return fp*1099511628211 + uint64(len(files))
}

// trigramCache stores per-(root,prefix,ext) trigram indices.
type trigramCache struct {
	m sync.Map // trigramCacheKey → *trigramCacheEntry
}

// getOrBuild returns a trigram index for the given key and file list.
//
// A change to the file SET (files added or removed since the cached index was
// built) is a HARD miss: it rebuilds synchronously and returns the fresh
// index. This is the #524 fix — a trigram index built from a partial FileTree
// snapshot during a reindex would otherwise be cached and judged "fresh" by
// Stale() (the files it knows about are unchanged, and it is within the TTL),
// so narrowing against it silently drops files that exist now and grep returns
// stable false-negative match counts until the TTL expires.
//
// For an unchanged file set, content/TTL staleness keeps the original
// non-blocking behaviour: a background rebuild is triggered and the stale
// index is returned for this call so a search never blocks on a rebuild.
func (c *trigramCache) getOrBuild(key trigramCacheKey, files []string) *trigram.Index {
	v, _ := c.m.LoadOrStore(key, &trigramCacheEntry{})
	entry, _ := v.(*trigramCacheEntry)
	fp := filesFingerprint(files)

	entry.mu.Lock()
	idx := entry.idx
	builtFP := entry.filesFP
	entry.mu.Unlock()

	if idx == nil || builtFP != fp {
		// First build, or the candidate set changed — build synchronously so
		// this call narrows against the current file set, never a stale one.
		entry.mu.Lock()
		if entry.idx == nil || entry.filesFP != fp {
			entry.idx = trigram.Build(files)
			entry.filesFP = fp
		}
		idx = entry.idx
		entry.mu.Unlock()
		return idx
	}

	if !idx.Stale() {
		return idx
	}

	// Same file set, but file contents/TTL went stale — trigger a background
	// rebuild and return the stale index now.
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
			entry.filesFP = fp
			entry.building = false
			entry.mu.Unlock()
		}()
	}

	return idx
}
