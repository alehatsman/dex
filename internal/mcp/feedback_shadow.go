package mcp

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/feedback"
)

// feedbackState is the in-process "throttle" substrate (#731): a live reader
// over the observe log plus a shadow logger. The reader turns the gauge `dex
// feedback` computes offline into a signal resident in the server process; the
// shadow logger records what the fused ranking WOULD be under a lane-agreement
// reweight, alongside the served ranking, for A/B.
//
// It NEVER changes what ask serves. The reweight stays in shadow until a
// measured open-rate win clears the data gate the issue records — the whole
// point is to gather that evidence before flipping any default. Everything here
// is gated behind DEX_FEEDBACK_SHADOW=1 and is a zero-cost no-op when off.
type feedbackState struct {
	fbOnce     sync.Once
	fbThrottle *feedback.Throttle
	fbShadow   *shadowLogger
}

// shadowEnabled reports whether shadow-mode feedback reweighting is on. Off by
// default: the default ask path never builds the reader or touches the log.
func shadowEnabled() bool { return os.Getenv("DEX_FEEDBACK_SHADOW") == "1" }

// feedbackThrottle lazily builds the live reader + shadow logger on first use
// when shadow mode is on, and returns (nil, nil) otherwise so callers can skip
// the whole path with one cheap branch.
func (s *Server) feedbackThrottle() (*feedback.Throttle, *shadowLogger) {
	if !shadowEnabled() {
		return nil, nil
	}
	s.fbOnce.Do(func() {
		if lp := feedback.DefaultLogPath(); lp != "" {
			s.fbThrottle = feedback.NewThrottle(lp, 0, 30*time.Second)
		}
		s.fbShadow = newShadowLogger(feedback.DefaultShadowLogPath())
	})
	return s.fbThrottle, s.fbShadow
}

// shadowRecord is one A/B comparison for a single ask: the served (static,
// LORO-calibrated #317) top-k vs the shadow (lane-agreement reweighted) top-k,
// the signal that drove the reweight, and a divergence summary. Operators
// accumulate these and only flip the reweight to live if shadow rankings
// correlate with a higher open-rate without an nDCG regression — the verdict
// `dex feedback --shadow` (feedback.AnalyzeShadow) computes.
//
// The type lives in internal/feedback so the writer here and the checker there
// share one definition (a second drifting copy is the #734 bug class).
type shadowRecord = feedback.ShadowRecord

// recordShadow computes the shadow ranking under the live lane-agreement
// reweight and logs it against the served order. hits is the served (static)
// order and is NOT mutated. No-op when shadow mode is off, the signal is empty,
// or there are too few hits to compare.
func (s *Server) recordShadow(intent string, hits []SemHit) {
	th, logger := s.feedbackThrottle()
	if th == nil || logger == nil || len(hits) < 2 {
		return
	}
	openRate, n := th.IntentSignal(intent)
	shadow := shadowReorder(hits, openRate, n)

	const topK = 5
	served := topPaths(hits, topK)
	shadowed := topPaths(shadow, topK)
	rec := shadowRecord{
		TS:           time.Now().Unix(),
		Intent:       intent,
		OpenRate:     openRate,
		N:            n,
		ServedTopK:   served,
		ShadowTopK:   shadowed,
		TopKJaccard:  jaccard(served, shadowed),
		MaxRankShift: maxRankShift(hits, shadow),
	}
	rec.Reordered = rec.MaxRankShift > 0
	logger.append(rec)
}

// shadowReorder re-scores each hit by its served score times the bounded
// lane-agreement multiplier and returns a new ordering. The input slice is left
// untouched. Ties preserve the served order (stable), so a zero multiplier
// (no signal) reproduces the served ranking exactly.
func shadowReorder(hits []SemHit, openRate float64, n int) []SemHit {
	type scored struct {
		hit   SemHit
		score float64
		idx   int
	}
	ss := make([]scored, len(hits))
	for i, h := range hits {
		m := feedback.ShadowMultiplier(openRate, n, len(h.Lanes))
		ss[i] = scored{hit: h, score: float64(h.Score) * m, idx: i}
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].score != ss[j].score {
			return ss[i].score > ss[j].score
		}
		return ss[i].idx < ss[j].idx
	})
	out := make([]SemHit, len(ss))
	for i := range ss {
		out[i] = ss[i].hit
	}
	return out
}

func topPaths(hits []SemHit, k int) []string {
	if len(hits) < k {
		k = len(hits)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = hits[i].Path
	}
	return out
}

// jaccard is the set overlap of two path lists: |A∩B| / |A∪B|. 1.0 means the
// shadow and served top-k hold the same files (order aside); lower means the
// reweight pulled different files into the top-k.
func jaccard(a, b []string) float64 {
	set := make(map[string]struct{}, len(a))
	for _, p := range a {
		set[p] = struct{}{}
	}
	inter := 0
	for _, p := range b {
		if _, ok := set[p]; ok {
			inter++
		}
		set[p] = struct{}{}
	}
	union := len(set)
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

// maxRankShift is the largest position change among paths present in both
// orderings — captures reordering the Jaccard set overlap misses.
func maxRankShift(served, shadow []SemHit) int {
	pos := make(map[string]int, len(shadow))
	for i, h := range shadow {
		if _, seen := pos[h.Path]; !seen {
			pos[h.Path] = i
		}
	}
	max := 0
	for i, h := range served {
		j, ok := pos[h.Path]
		if !ok {
			continue
		}
		if d := i - j; d > max {
			max = d
		} else if d := j - i; d > max {
			max = d
		}
	}
	return max
}

// shadowLogger appends shadowRecords to a JSONL file, best-effort: a write
// error never propagates into the ask path. A nil logger or empty path is a
// silent no-op.
type shadowLogger struct {
	mu   sync.Mutex
	path string
}

func newShadowLogger(path string) *shadowLogger { return &shadowLogger{path: path} }

func (l *shadowLogger) append(rec shadowRecord) {
	if l == nil || l.path == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(b, '\n'))
}
