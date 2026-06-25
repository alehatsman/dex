// Package gitrecency provides TTL-cached git signals for boosting search results
// toward recently-modified and currently-dirty files.
package gitrecency

import (
	"math"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	recencyTTL  = 60 * time.Second
	dirtyTTL    = 10 * time.Second
	halfLifeHrs = 48.0 // hours; exponential decay half-life for git recency
	// RecencyBoostMax is the maximum additive boost multiplier (applied at age=0),
	// expressed as a fraction of 1/rrfK so callers can scale it uniformly.
	RecencyBoostMax = float32(0.25)
	// DirtyBoostVal is the additive boost multiplier for currently-dirty files.
	DirtyBoostVal = float32(0.10)
)

// Cache provides TTL-backed git recency and dirty signals.
// The zero value is safe — all boost methods return nil.
// New(root) creates a live cache that runs git commands against root.
type Cache struct {
	root string // project root; empty → no-op

	recMu      sync.Mutex
	recency    map[string]float32 // relative path → decayed boost (0..RecencyBoostMax)
	recencyExp time.Time

	dirMu    sync.Mutex
	dirty    map[string]struct{} // relative path → dirty
	dirtyExp time.Time
}

// New creates a Cache rooted at root. root should be the project root where .git lives.
func New(root string) *Cache {
	return &Cache{root: root}
}

// Bonus returns a per-chunk-ID additive RRF bonus for git recency and dirty signals.
// Bonuses are expressed as fractions of 1/rrfK so callers can apply the same
// FusionLinear scale factor as session proximity bonuses.
// Returns nil when the cache has no root, or all bonuses are zero.
func (c *Cache) Bonus(pathFor map[int64]string, rrfK int) map[int64]float32 {
	if c == nil || c.root == "" || len(pathFor) == 0 || rrfK <= 0 {
		return nil
	}
	rec := c.recencyMap()
	dir := c.dirtySet()
	k := float32(rrfK)

	var out map[int64]float32
	for id, p := range pathFor {
		bonus := float32(0)
		if rb, ok := rec[p]; ok {
			bonus += rb / k
		}
		if _, ok := dir[p]; ok {
			bonus += DirtyBoostVal / k
		}
		if bonus > 0 {
			if out == nil {
				out = make(map[int64]float32, len(pathFor))
			}
			out[id] = bonus
		}
	}
	return out
}

func (c *Cache) recencyMap() map[string]float32 {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	if time.Now().Before(c.recencyExp) && c.recency != nil {
		return c.recency
	}
	c.recency = refreshRecency(c.root)
	c.recencyExp = time.Now().Add(recencyTTL)
	return c.recency
}

func (c *Cache) dirtySet() map[string]struct{} {
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	if time.Now().Before(c.dirtyExp) && c.dirty != nil {
		return c.dirty
	}
	c.dirty = refreshDirty(c.root)
	c.dirtyExp = time.Now().Add(dirtyTTL)
	return c.dirty
}

// refreshRecency runs git log and returns a map of relative path → decayed boost.
// Paths appear once per file, keyed on their most recent commit timestamp.
// Degrades gracefully to an empty map when git is unavailable or returns an error.
func refreshRecency(root string) map[string]float32 {
	now := time.Now()
	out := make(map[string]float32)

	cmd := exec.Command("git", "log", "--name-only",
		"--since=14.days", "--pretty=format:%ct", "--diff-filter=AM")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return out
	}

	var curTS int64
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if ts, err := strconv.ParseInt(line, 10, 64); err == nil {
			curTS = ts
			continue
		}
		if curTS == 0 {
			continue
		}
		// Only the most-recent commit time is used per file.
		if _, seen := out[line]; seen {
			continue
		}
		age := now.Sub(time.Unix(curTS, 0))
		if age < 0 {
			age = 0
		}
		// Exponential decay: boost = RecencyBoostMax × 2^(−age / 48h)
		decay := math.Pow(2, -age.Hours()/halfLifeHrs)
		out[line] = float32(float64(RecencyBoostMax) * decay)
	}
	return out
}

// refreshDirty runs git status --porcelain and returns the set of relative
// paths that are currently modified, added, or untracked.
// Degrades gracefully to an empty set when git is unavailable.
func refreshDirty(root string) map[string]struct{} {
	out := make(map[string]struct{})
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, raw := range strings.Split(string(b), "\n") {
		if len(raw) < 4 {
			continue
		}
		// Format: "XY path" where XY are two status columns and path starts at col 3.
		path := strings.TrimSpace(raw[3:])
		// Handle renames: "old -> new"
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		if path != "" {
			out[path] = struct{}{}
		}
	}
	return out
}
