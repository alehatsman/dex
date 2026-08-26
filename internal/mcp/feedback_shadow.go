package mcp

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/feedback"
)

// feedbackState is the in-process "throttle" substrate (#731): a live reader
// over the observe log plus an optional shadow logger. The reader turns the
// gauge `dex feedback` computes offline into a signal resident in the server.
//
// Two gates:
//   - DEX_FEEDBACK_SHADOW=1: log static-vs-reweighted ranking per ask (A/B).
//   - DEX_FEEDBACK_LIVE:     serves the reweighted hits. Default ON (#220 —
//     the #783 data gate cleared 2026-06-27: nDCG non-regression confirmed,
//     shadow WIN-CANDIDATE; opt-in via env had left most sessions with zero
//     benefit from data dex was already collecting). Set to "0" to opt out.
//
// Both are zero-cost no-ops when off. Shadow logging continues in live mode so
// dex feedback --shadow keeps tracking static-vs-reweighted as a regression
// check.
type feedbackState struct {
	fbOnce     sync.Once
	fbThrottle *feedback.Throttle
	fbShadow   *shadowLogger
}

// shadowEnabled reports whether shadow-mode A/B logging is on.
func shadowEnabled() bool { return os.Getenv("DEX_FEEDBACK_SHADOW") == "1" }

// liveOffValues are the DEX_FEEDBACK_LIVE values that opt out of the default-on
// live reweight (#220). Recognizing the common falsy spellings — not just "0" —
// avoids silently re-enabling the feature for an operator who reasonably typed
// "false" or "off" expecting it to stick.
var liveOffValues = map[string]bool{"0": true, "false": true, "off": true, "no": true}

// liveEnabled reports whether the lane-agreement reweight is served to
// callers. Default ON (#220) — set DEX_FEEDBACK_LIVE to any of liveOffValues
// to opt out.
func liveEnabled() bool {
	return !liveOffValues[strings.ToLower(strings.TrimSpace(os.Getenv("DEX_FEEDBACK_LIVE")))]
}

// feedbackThrottle lazily builds the live reader (and shadow logger when shadow
// is on) on first use when either shadow or live mode is active. Returns
// (nil, nil) when both are off so callers can skip the whole path cheaply.
func (s *Server) feedbackThrottle() (*feedback.Throttle, *shadowLogger) {
	if !shadowEnabled() && !liveEnabled() {
		return nil, nil
	}
	s.fbOnce.Do(func() {
		if lp := feedback.DefaultLogPath(); lp != "" {
			s.fbThrottle = feedback.NewThrottle(lp, 0, 30*time.Second)
		}
		if shadowEnabled() {
			s.fbShadow = newShadowLogger(feedback.DefaultShadowLogPath())
		}
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
func (s *Server) recordShadow(intent, question string, hits []SemHit) {
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
		Query:        question,
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

// applyLiveReweight returns hits re-scored by the live lane-agreement signal
// when DEX_FEEDBACK_LIVE is on, and the original slice otherwise. Designed to
// be called immediately after recordShadow so the shadow log captures the
// static-vs-reweighted delta even during live serving.
func (s *Server) applyLiveReweight(intent string, hits []SemHit) []SemHit {
	openRate, n := s.graphIntentSignal(intent)
	if n == 0 {
		return hits
	}
	return shadowReorder(hits, openRate, n)
}

// graphIntentSignal returns the live open-rate signal for an intent — the
// same per-intent signal applyLiveReweight uses for semantic hits, also used
// by callEdges (graph_callers/graph_callees) to extend the reweight to graph
// lanes (#220; graphImpact is not yet wired — see server_graph_calls.go).
// Returns (0, 0) when live reweight is off or the throttle can't be built;
// feedback.ShadowMultiplier(0, 0, ...) degrades to the identity multiplier
// (1.0), so callers can invoke this unconditionally without a separate
// liveEnabled() branch.
func (s *Server) graphIntentSignal(intent string) (openRate float64, n int) {
	if !liveEnabled() {
		return 0, 0
	}
	th, _ := s.feedbackThrottle()
	if th == nil {
		return 0, 0
	}
	return th.IntentSignal(intent)
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

// topPaths returns the first k DISTINCT file paths in hit order. A single file
// surfaces as several chunk hits; the top-k is about which FILES the ranking
// would suggest, so duplicates collapse to their highest-ranked occurrence.
// Without the dedup the open-rate denominator inflates — five slots for one
// file — and a single opened file counts up to k times (#743).
func topPaths(hits []SemHit, k int) []string {
	out := make([]string, 0, k)
	seen := make(map[string]struct{}, k)
	for _, h := range hits {
		if _, dup := seen[h.Path]; dup {
			continue
		}
		seen[h.Path] = struct{}{}
		out = append(out, h.Path)
		if len(out) == k {
			break
		}
	}
	return out
}

// jaccard is the set overlap of two path lists: |A∩B| / |A∪B|. 1.0 means the
// shadow and served top-k hold the same files (order aside); lower means the
// reweight pulled different files into the top-k. Robust to duplicate entries
// in either list (counts distinct files only — see #743).
func jaccard(a, b []string) float64 {
	setA := make(map[string]struct{}, len(a))
	for _, p := range a {
		setA[p] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, p := range b {
		setB[p] = struct{}{}
	}
	inter := 0
	for p := range setB {
		if _, ok := setA[p]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
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
