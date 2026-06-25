package mcp

import (
	"sync"
	"time"
)

// bounceWindow is how long after a compressed delivery we watch for a
// re-read of the same file. Agent turns are typically <30 s; 60 s is a
// conservative window that catches retry loops without affecting normal use.
const bounceWindow = 60 * time.Second

// bounceTracker detects the "compression thrash" failure mode: an agent
// requests a file, receives a compressed view (signatures/map), then
// immediately requests the same file again because the compressed version
// wasn't enough. On the second request, shouldForceFull returns true and
// the caller must bypass compression for that delivery.
//
// State is session-scoped via sessionID to avoid cross-session interference.
// The tracker is safe for concurrent use.
type bounceTracker struct {
	mu         sync.Mutex
	compressed map[string]time.Time // "sessionID:path" → when we last delivered compressed
	bounced    map[string]bool      // "sessionID:path" → force-full on next read (single-use)
}

func newBounceTracker() *bounceTracker {
	return &bounceTracker{
		compressed: make(map[string]time.Time),
		bounced:    make(map[string]bool),
	}
}

func bounceKey(sessionID, path string) string { return sessionID + "\x00" + path }

// recordCompressed notes that sessionID received a compressed view of path.
func (bt *bounceTracker) recordCompressed(sessionID, path string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	bt.compressed[bounceKey(sessionID, path)] = time.Now()
}

// recordRead checks whether path was recently compressed for sessionID.
// If so, marks it for a forced-full upgrade on the next shouldForceFull call.
func (bt *bounceTracker) recordRead(sessionID, path string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	k := bounceKey(sessionID, path)
	if t, ok := bt.compressed[k]; ok && time.Since(t) < bounceWindow {
		bt.bounced[k] = true
	}
}

// shouldForceFull returns true if path was bounced in this session and
// clears the bounce flag (single-use: the next read gets full, subsequent
// reads reset normally).
func (bt *bounceTracker) shouldForceFull(sessionID, path string) bool {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	k := bounceKey(sessionID, path)
	if bt.bounced[k] {
		delete(bt.bounced, k)
		delete(bt.compressed, k)
		return true
	}
	return false
}

// viewDowngradeChain returns the ordered sequence of modes to try when the
// requested mode is too expensive. The chain always ends in ReadModeHandle
// which is guaranteed cheap (~25 tokens).
func viewDowngradeChain(requested ReadMode) []ReadMode {
	switch requested {
	case ReadModeFull:
		return []ReadMode{ReadModeFull, ReadModeSkeleton, ReadModeSignatures, ReadModeMap, ReadModeHandle}
	case ReadModeSkeleton:
		return []ReadMode{ReadModeSkeleton, ReadModeSignatures, ReadModeMap, ReadModeHandle}
	case ReadModeAggressive:
		return []ReadMode{ReadModeAggressive, ReadModeSignatures, ReadModeMap, ReadModeHandle}
	case ReadModeSignatures:
		return []ReadMode{ReadModeSignatures, ReadModeMap, ReadModeHandle}
	case ReadModeMap:
		return []ReadMode{ReadModeMap, ReadModeHandle}
	default:
		return []ReadMode{requested, ReadModeHandle}
	}
}

// estimateModeTokens estimates the token cost of reading a file with the
// given mode, based on the file's raw token count. Fractions are from
// empirical lean-ctx ablation / dex compression ratios.
func estimateModeTokens(fileTokens int, mode ReadMode) int {
	switch mode {
	case ReadModeFull:
		return fileTokens
	case ReadModeAggressive:
		return fileTokens * 40 / 100
	case ReadModeSkeleton:
		return fileTokens * 30 / 100
	case ReadModeSignatures:
		return fileTokens * 20 / 100
	case ReadModeMap:
		return fileTokens * 12 / 100
	case "reference":
		return fileTokens * 5 / 100
	case ReadModeHandle:
		return 25
	}
	return fileTokens
}

// selectAffordableMode returns the richest view mode from the downgrade
// chain that fits within budgetTokens. fileTokens is the file's estimated
// raw token count. Returns requestedMode unchanged when budgetTokens <= 0
// (opt-in: callers that don't set a budget get the original behavior).
func selectAffordableMode(requestedMode ReadMode, fileTokens, budgetTokens int) ReadMode {
	if budgetTokens <= 0 {
		return requestedMode
	}
	for _, m := range viewDowngradeChain(requestedMode) {
		if estimateModeTokens(fileTokens, m) <= budgetTokens {
			return m
		}
	}
	return ReadModeHandle
}
