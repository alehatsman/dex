package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// loopWindow is the rolling window for counting repeated calls.
const loopWindow = 5 * time.Minute

// Loop detection thresholds (calls within loopWindow).
const (
	loopHintThreshold   = 2  // add guidance hint
	loopReduceThreshold = 4  // trim response + hint
	loopBlockThreshold  = 6  // block the call entirely
	searchGroupLimit    = 10 // total search calls (any type) across the window
)

// loopBlockedMsg is returned when a call is blocked.
const loopBlockedMsg = "loop-blocked: this exact call has been made %d times in the last 5 minutes. " +
	"Consider: search_grep for an exact literal, file_tree to orient, narrowing with path= or kind= " +
	"parameters, or storing findings with the knowledge tool."

// loopHintMsg is a softer nudge for moderate repetition.
const loopHintMsg = "repeated call (%d times) — if results aren't helping, try search_grep for " +
	"exact text, file_tree for orientation, or knowledge action=add to store findings."

// ThrottleLevel describes the loop detector's response to a call.
type ThrottleLevel int

const (
	ThrottleNormal ThrottleLevel = iota // no action
	ThrottleHint                        // add hint to response
	ThrottleReduce                      // trim response + hint
	ThrottleBlock                       // reject the call
)

type loopEntry struct {
	timestamps []time.Time
}

// loopDetector tracks per-session per-fingerprint call history and a
// cross-tool search group budget.
type loopDetector struct {
	mu     sync.Mutex
	calls  map[string]*loopEntry // fingerprint → timestamps
	search []time.Time           // timestamps of all search-class calls
}

func newLoopDetector() *loopDetector {
	return &loopDetector{
		calls: make(map[string]*loopEntry),
	}
}

// fingerprint computes a stable 8-byte hex key from tool name + JSON args.
func fingerprint(tool, args string) string {
	h := sha256.Sum256([]byte(tool + ":" + args))
	return hex.EncodeToString(h[:4])
}

// argsKey serialises an arbitrary value to a canonical JSON string for fingerprinting.
func argsKey(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// purge removes timestamps older than loopWindow. Must be called with mu held.
func (d *loopDetector) purge(now time.Time) {
	cutoff := now.Add(-loopWindow)
	for fp, e := range d.calls {
		var fresh []time.Time
		for _, t := range e.timestamps {
			if t.After(cutoff) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			delete(d.calls, fp)
		} else {
			e.timestamps = fresh
		}
	}
	var freshSearch []time.Time
	for _, t := range d.search {
		if t.After(cutoff) {
			freshSearch = append(freshSearch, t)
		}
	}
	d.search = freshSearch
}

// Check records a call and returns the throttle level and a hint string.
// isSearch must be true for search-class tools (search_semantic, ask, search_symbol).
func (d *loopDetector) Check(tool, args string, isSearch bool) (ThrottleLevel, string) {
	now := time.Now()
	fp := fingerprint(tool, args)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.purge(now)

	// Record the call.
	e := d.calls[fp]
	if e == nil {
		e = &loopEntry{}
		d.calls[fp] = e
	}
	e.timestamps = append(e.timestamps, now)
	count := len(e.timestamps)

	if isSearch {
		d.search = append(d.search, now)
	}
	searchGroupCount := len(d.search)

	// Cross-tool search group budget exceeded.
	if isSearch && searchGroupCount >= searchGroupLimit {
		return ThrottleBlock, fmt.Sprintf(
			"search-group-blocked: %d search calls in the last 5 minutes. Store findings with "+
				"knowledge action=add, use search_grep for exact literals, or read specific files directly.",
			searchGroupCount,
		)
	}

	switch {
	case count >= loopBlockThreshold:
		return ThrottleBlock, fmt.Sprintf(loopBlockedMsg, count)
	case count >= loopReduceThreshold:
		return ThrottleReduce, fmt.Sprintf(loopHintMsg, count)
	case count >= loopHintThreshold:
		return ThrottleHint, fmt.Sprintf(loopHintMsg, count)
	}
	return ThrottleNormal, ""
}
