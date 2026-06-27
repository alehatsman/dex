package feedback

import (
	"os"
	"sync"
	"time"
)

// Throttle is the in-process live reader (#731): it tails hooks.jsonl and holds
// a cached, periodically-refreshed Report so the ask path can read the current
// relevance signal without re-parsing the log on every call. It is the
// "gauge → throttle" substrate — the same join the `dex feedback` CLI runs,
// but resident in the server process.
//
// It is read-only over the log and serves a snapshot; it does not itself change
// retrieval. The shadow reweighter (ShadowMultiplier) turns its signal into a
// candidate lane weighting that callers compute alongside the served ranking
// and log for A/B, never serving it until a measured open-rate win clears the
// data gate the issue records.
type Throttle struct {
	logPath string
	window  int
	minAge  time.Duration // minimum interval between log re-reads

	mu       sync.Mutex
	cached   Report
	loadedAt time.Time
	logMod   time.Time
}

// NewThrottle builds a live reader over logPath. window matches the CLI's
// lookahead bound (0 = whole session). The log is read lazily on the first
// Snapshot and re-read at most once per minAge, and only when the file's mtime
// advanced — a cheap stat guards the parse.
func NewThrottle(logPath string, window int, minAge time.Duration) *Throttle {
	if minAge <= 0 {
		minAge = 30 * time.Second
	}
	return &Throttle{logPath: logPath, window: window, minAge: minAge}
}

// Snapshot returns the current joined report, refreshing from disk when the
// cache is older than minAge AND the log's mtime advanced since the last load.
// A read error leaves the previous snapshot in place (fail-soft: a missing or
// truncated log never breaks the ask path).
func (t *Throttle) Snapshot() Report {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if !t.loadedAt.IsZero() && now.Sub(t.loadedAt) < t.minAge {
		return t.cached
	}

	fi, err := os.Stat(t.logPath)
	if err != nil {
		t.loadedAt = now // back off; don't stat-storm a missing log
		return t.cached
	}
	if !t.loadedAt.IsZero() && !fi.ModTime().After(t.logMod) {
		t.loadedAt = now // unchanged since last load — keep cache
		return t.cached
	}

	events, err := ReadLog(t.logPath)
	if err != nil {
		t.loadedAt = now
		return t.cached
	}
	rep := Compute(events, t.window)
	rep.LogPath = t.logPath
	t.cached = rep
	t.loadedAt = now
	t.logMod = fi.ModTime()
	return t.cached
}

// IntentSignal returns the open-rate and ask sample count for an intent from
// the latest snapshot. n is the number of asks routed to that intent — the
// confidence the reweighter shrinks toward zero at low counts.
func (t *Throttle) IntentSignal(intent string) (openRate float64, n int) {
	rep := t.Snapshot()
	st, ok := rep.ByIntent[intent]
	if !ok {
		return 0, 0
	}
	return st.OpenRate, st.Asks
}

// Reweight parameters. These bound the shadow nudge so it can never overpower
// the LORO-calibrated static prior (#317): the multiplier lives in
// [1, 1+MaxBoost], shrinks toward 1 as the sample count falls (confidence
// smoothing), and is zero for single-lane hits.
const (
	// reweightKappa scales the raw nudge. 0.5 keeps the boost well inside
	// MaxBoost across the plausible (miss, confidence, laneCount) range.
	reweightKappa = 0.5
	// reweightMaxBoost hard-caps the multiplier at 1.5×, so a reweighted lane
	// can never dominate the static ranking by more than a bounded margin.
	reweightMaxBoost = 0.5
	// reweightSmoothing is the pseudo-count in the confidence shrinkage
	// n/(n+smoothing). At n=1 (the post-#734 reality) confidence ≈ 0.05, so
	// the shadow ranking is effectively identical to static until real traffic
	// accrues — faithful to "no signal yet" rather than thrashing on noise.
	reweightSmoothing = 20.0
)

// ShadowMultiplier is the bounded, decaying reweight applied to a candidate hit
// in shadow mode. The hypothesis (the issue's "shadow first, flip only on a
// measured win" — this is the candidate under test, never served):
//
// when an intent has a LOW open-rate, the agent is ignoring what ask points at,
// so prefer hits that MULTIPLE lanes agreed on — cross-lane agreement is the
// codebase's own confidence signal (a hit in vector∩bm25 is stronger than a
// single-lane hit). The boost therefore scales with (laneCount-1):
//
//	mult = 1 + kappa · miss · confidence · (laneCount-1),  clamped to [1, 1+MaxBoost]
//
// where miss = 1-openRate and confidence = n/(n+smoothing). The signal can't
// attribute a miss to a SPECIFIC lane (hooks.jsonl records opened paths, not
// which lane surfaced them), so this is deliberately a lane-AGREEMENT nudge,
// not a per-lane up/down-weight — the only direction the recorded signal can
// honestly support. laneCount <= 1 returns 1.0 (no change).
func ShadowMultiplier(openRate float64, n, laneCount int) float64 {
	if laneCount <= 1 || n <= 0 {
		return 1.0
	}
	miss := 1.0 - openRate
	if miss < 0 {
		miss = 0
	}
	confidence := float64(n) / (float64(n) + reweightSmoothing)
	boost := reweightKappa * miss * confidence * float64(laneCount-1)
	if boost > reweightMaxBoost {
		boost = reweightMaxBoost
	}
	if boost < 0 {
		boost = 0
	}
	return 1.0 + boost
}
