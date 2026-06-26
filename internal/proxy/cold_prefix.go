package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// coldThreshold is the minimum gap after which the provider's prompt cache
	// is considered expired. Anthropic's TTL is 5 minutes; 2× (600 s) is the
	// conservative floor so brief pauses don't falsely trigger repack.
	coldThreshold = 600 * time.Second

	// touchMinInterval throttles disk writes so a busy turn loop doesn't hammer
	// the filesystem on every request.
	touchMinInterval = 30 * time.Second
)

// coldTouchState is the on-disk representation persisted to the touch file.
type coldTouchState struct {
	TS        time.Time `json:"ts"`
	Repacking bool      `json:"repacking"`
}

// ColdPrefixTracker detects when the provider's prompt cache has likely expired
// (no activity for > coldThreshold) and latches a repacking flag so all
// subsequent turns use a smaller, cache-stable prefix.
//
// State is persisted to ~/.cache/dex/proxy/cold_prefix_touch.json with atomic
// writes (write-rename). Fail-open: any I/O error leaves the tracker in an
// empty state (no prior touch), deferring the first repack detection to the
// next natural cold gap.
type ColdPrefixTracker struct {
	mu        sync.Mutex
	path      string
	lastTouch time.Time // zero = no prior touch recorded
	repacking bool
	lastWrite time.Time // last time state was flushed to disk
}

// NewColdPrefixTracker returns a tracker backed by the default state file.
func NewColdPrefixTracker() *ColdPrefixTracker {
	t := &ColdPrefixTracker{path: coldPrefixTouchPath()}
	t.load()
	return t
}

func coldPrefixTouchPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "dex", "proxy", "cold_prefix_touch.json")
}

// load reads persisted state from disk. Errors are silently ignored (fail-open).
func (t *ColdPrefixTracker) load() {
	if t.path == "" {
		return
	}
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var st coldTouchState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	t.lastTouch = st.TS
	t.repacking = st.Repacking
	// Pretend we just wrote at the loaded timestamp to suppress an
	// immediate redundant write on the first Touch() call.
	t.lastWrite = st.TS
}

// ShouldRepack returns true when there is at least one prior touch and the
// elapsed time since it exceeds coldThreshold. Returns false once repacking is
// already latched (SetRepacking should only be called once per latch event).
func (t *ColdPrefixTracker) ShouldRepack() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastTouch.IsZero() || t.repacking {
		return false
	}
	return time.Since(t.lastTouch) >= coldThreshold
}

// SetRepacking latches the repacking flag and flushes to disk immediately
// (not subject to the write-throttle).
func (t *ColdPrefixTracker) SetRepacking() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.repacking {
		t.repacking = true
		t.writeFileLocked()
	}
}

// IsRepacking reports whether the repacking flag is latched.
func (t *ColdPrefixTracker) IsRepacking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.repacking
}

// Touch records now as the last-active timestamp. Disk writes are throttled
// to at most one per touchMinInterval; the repacking latch bypasses this.
func (t *ColdPrefixTracker) Touch() {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTouch = now
	if now.Sub(t.lastWrite) >= touchMinInterval {
		t.writeFileLocked()
	}
}

// writeFileLocked writes the current state atomically (write-to-temp then
// rename). Caller must hold t.mu.
func (t *ColdPrefixTracker) writeFileLocked() {
	if t.path == "" {
		return
	}
	data, err := json.Marshal(coldTouchState{TS: t.lastTouch, Repacking: t.repacking})
	if err != nil {
		return
	}
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, t.path)
	t.lastWrite = time.Now()
}
