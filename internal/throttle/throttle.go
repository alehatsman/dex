// Package throttle detects repeated tool calls within a rolling window and
// recommends a response level (hint, reduce, block) so a spinning agent gets
// nudged toward a different strategy. It is transport-agnostic: callers pass a
// tool name and a canonical args key, and act on the returned Level.
package throttle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// window is the rolling window for counting repeated calls.
const window = 5 * time.Minute

// Detection thresholds (calls within window).
const (
	hintThreshold    = 2  // add guidance hint
	reduceThreshold  = 4  // trim response + hint
	blockThreshold   = 6  // block the call entirely
	searchGroupLimit = 10 // total search calls (any type) across the window
)

// blockedMsg is returned when a call is blocked.
const blockedMsg = "loop-blocked: this exact call has been made %d times in the last 5 minutes. " +
	"Consider: grep for an exact literal, ls to orient, or narrowing with path= or kind= " +
	"parameters."

// hintMsg is a softer nudge for moderate repetition.
const hintMsg = "repeated call (%d times) — if results aren't helping, try grep for " +
	"exact text, ls for orientation, or read specific files directly."

// Level describes the detector's recommended response to a call.
type Level int

const (
	Normal Level = iota // no action
	Hint                // add hint to response
	Reduce              // trim response + hint
	Block               // reject the call
)

type entry struct {
	timestamps []time.Time
}

// Detector tracks per-fingerprint call history and a cross-tool search group
// budget.
type Detector struct {
	mu     sync.Mutex
	calls  map[string]*entry // fingerprint → timestamps
	search []time.Time       // timestamps of all search-class calls
}

// New returns an empty Detector.
func New() *Detector {
	return &Detector{
		calls: make(map[string]*entry),
	}
}

// Fingerprint computes a stable 8-byte hex key from tool name + JSON args.
func Fingerprint(tool, args string) string {
	h := sha256.Sum256([]byte(tool + ":" + args))
	return hex.EncodeToString(h[:4])
}

// ArgsKey serialises an arbitrary value to a canonical JSON string for fingerprinting.
func ArgsKey(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// purge removes timestamps older than window. Must be called with mu held.
func (d *Detector) purge(now time.Time) {
	cutoff := now.Add(-window)
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

// Check records a call and returns the recommended level and a hint string.
// isSearch must be true for search-class tools (search_semantic, ask, search_symbol).
func (d *Detector) Check(tool, args string, isSearch bool) (Level, string) {
	now := time.Now()
	fp := Fingerprint(tool, args)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.purge(now)

	// Record the call.
	e := d.calls[fp]
	if e == nil {
		e = &entry{}
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
		return Block, fmt.Sprintf(
			"search-group-blocked: %d search calls in the last 5 minutes. "+
				"Use grep for exact literals, or read specific files directly.",
			searchGroupCount,
		)
	}

	switch {
	case count >= blockThreshold:
		return Block, fmt.Sprintf(blockedMsg, count)
	case count >= reduceThreshold:
		return Reduce, fmt.Sprintf(hintMsg, count)
	case count >= hintThreshold:
		return Hint, fmt.Sprintf(hintMsg, count)
	}
	return Normal, ""
}
