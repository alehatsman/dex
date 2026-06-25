package compress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// badnessThreshold is the output-ratio below which a mode is considered
	// to have produced unhelpful output (agent barely responded after reading).
	badnessThreshold = 0.05

	// penaltyThreshold is the EMA penalty above which a mode is skipped
	// by ChooseMode and the next cheaper mode is tried instead.
	penaltyThreshold = 0.3

	// emaDecay is the exponential moving average decay factor applied to the
	// existing penalty on each observation. 0.9 means ~10 events to forget.
	emaDecay = 0.9

	// policyFile is the filename within the per-project cache directory.
	policyFile = "adaptive_policy.json"

	// signalLR is the learning rate applied to signal weights when adjusting
	// per-extension deltas.
	signalLR = 0.05

	// signalDeltaClamp is the maximum absolute value of a per-extension delta.
	signalDeltaClamp = 0.15
)

// Signal classifies a feedback event that adjusts the per-extension learned delta.
type Signal int

const (
	// SignalEditFail fires when an agent read a file compressed and a subsequent
	// Edit on that file returned an error (old_string not found, stale line nums).
	// Weight: −6.0 × signalLR per occurrence, clamped at −signalDeltaClamp.
	SignalEditFail Signal = iota
)

// signalWeights maps each Signal to its raw adjustment weight. Negative values
// increase the ext penalty (push toward less compression); positive decay it.
var signalWeights = map[Signal]float64{
	SignalEditFail: -6.0,
}

// modeFallbacks defines the downgrade chain: if a mode is penalized, try
// the next mode in the slice instead.
var modeFallbacks = []string{"aggressive", "signatures", "map", "full"}

// IntentFromTask maps a free-text task description to a coarse intent
// label used as the key in the penalty table. Mirrors the keyword sets
// in TaskToMode so feedback is associated with the same buckets.
func IntentFromTask(task string) string {
	lower := strings.ToLower(task)
	switch {
	// test before generate: "write unit tests" should match test, not generate.
	case containsAnyAdaptive(lower, []string{"add test", "write test", "unit test", "integrat test", "test case", "benchmark"}):
		return "test"
	case containsAnyAdaptive(lower, []string{"generat", "implement", "add feat", "write", "creat", "scaffold", "build out", "code up"}):
		return "generate"
	case containsAnyAdaptive(lower, []string{"refactor", "clean", "reorganiz", "restructur", "rename"}):
		return "refactor"
	case containsAnyAdaptive(lower, []string{"review", "audit", "check", "inspect", "analyz"}):
		return "review"
	// search before debug: "find where error is logged" is a search, not a debug.
	case containsAnyAdaptive(lower, []string{"search", "find", "where", "look", "locat"}):
		return "search"
	case containsAnyAdaptive(lower, []string{"fix", "bug", "debug", "error", "fail", "broken", "crash"}):
		return "debug"
	default:
		return "read"
	}
}

func containsAnyAdaptive(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// PolicyTable tracks per-(intent, mode) EMA penalty scores and selects
// the best available mode for a given intent. Loaded from and saved to
// <cacheDir>/adaptive_policy.json.
type PolicyTable struct {
	mu       sync.Mutex
	cacheDir string
	// penalties[intent][mode] = EMA penalty in [0, 1].
	penalties map[string]map[string]float64
	// extDeltas[ext] = learned delta in [-signalDeltaClamp, +signalDeltaClamp].
	// Negative values mean compressed reads on that ext caused edit failures;
	// callers should prefer less-aggressive modes for those file types.
	extDeltas map[string]float64
}

// policyJSON is the on-disk representation.
type policyJSON struct {
	Penalties map[string]map[string]float64 `json:"penalties"`
	ExtDeltas map[string]float64            `json:"ext_deltas,omitempty"`
}

// LoadPolicy loads the policy from cacheDir/adaptive_policy.json, creating
// an empty table when the file does not exist.
func LoadPolicy(cacheDir string) *PolicyTable {
	pt := &PolicyTable{
		cacheDir:  cacheDir,
		penalties: make(map[string]map[string]float64),
		extDeltas: make(map[string]float64),
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, policyFile))
	if err != nil {
		return pt // not found or unreadable — start fresh
	}
	var pj policyJSON
	if err := json.Unmarshal(data, &pj); err != nil {
		return pt // corrupt — start fresh
	}
	if pj.Penalties != nil {
		pt.penalties = pj.Penalties
	}
	if pj.ExtDeltas != nil {
		pt.extDeltas = pj.ExtDeltas
	}
	return pt
}

// RecordFeedback updates the penalty for (intent, mode) based on the
// agent's output ratio for that turn. Low output ratio → high badness →
// penalty accumulates. High ratio → badness=0 → penalty decays toward 0.
func (pt *PolicyTable) RecordFeedback(intent, mode string, outputRatio float64) {
	if intent == "" || mode == "" {
		return
	}
	badness := 0.0
	if outputRatio < badnessThreshold {
		badness = (badnessThreshold - outputRatio) * 20 // max 1.0 at ratio=0
	}

	pt.mu.Lock()
	defer pt.mu.Unlock()

	if pt.penalties[intent] == nil {
		pt.penalties[intent] = make(map[string]float64)
	}
	current := pt.penalties[intent][mode]
	pt.penalties[intent][mode] = current*emaDecay + badness*(1-emaDecay)

	_ = pt.save() // best-effort
}

// ChooseMode returns the best mode for intent given the predicted (default)
// mode. If the predicted mode's penalty exceeds penaltyThreshold, it walks
// the fallback chain (aggressive→signatures→map→full) and returns the first
// mode whose penalty is below threshold. Returns predicted if no penalty data
// exists or all modes are penalized.
func (pt *PolicyTable) ChooseMode(intent, predicted string) string {
	if intent == "" || predicted == "" {
		return predicted
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()

	intentPenalties := pt.penalties[intent]
	if intentPenalties == nil {
		return predicted
	}

	if intentPenalties[predicted] <= penaltyThreshold {
		return predicted // predicted is fine
	}

	// Walk fallback chain from the predicted mode toward "full" (least lossy).
	startIdx := -1
	for i, m := range modeFallbacks {
		if m == predicted {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return predicted // unknown mode — can't walk fallbacks
	}
	for i := startIdx + 1; i < len(modeFallbacks); i++ {
		m := modeFallbacks[i]
		if intentPenalties[m] <= penaltyThreshold {
			return m
		}
	}
	return predicted // all penalized — stick with original
}

// RecordSignal records a learning signal for the file at path, adjusting the
// per-extension delta toward more-complete reads (negative signals like
// SignalEditFail) or more compression (positive signals). The adjustment is
// weight × signalLR, clamped to ±signalDeltaClamp.
func (pt *PolicyTable) RecordSignal(path string, sig Signal) {
	ext := filepath.Ext(path)
	if ext == "" {
		return
	}
	w, ok := signalWeights[sig]
	if !ok {
		return
	}
	adj := w * signalLR

	pt.mu.Lock()
	defer pt.mu.Unlock()

	if pt.extDeltas == nil {
		pt.extDeltas = make(map[string]float64)
	}
	d := pt.extDeltas[ext] + adj
	if d > signalDeltaClamp {
		d = signalDeltaClamp
	} else if d < -signalDeltaClamp {
		d = -signalDeltaClamp
	}
	pt.extDeltas[ext] = d
	_ = pt.save()
}

// ExtDelta returns the per-extension learned delta for the file at path.
// Negative values indicate compressed reads on that extension have caused
// edit failures; callers should prefer less-aggressive modes. Returns 0 for
// unseen extensions or when path has no extension.
func (pt *PolicyTable) ExtDelta(path string) float64 {
	ext := filepath.Ext(path)
	if ext == "" {
		return 0
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.extDeltas[ext]
}

// save writes the current penalty table to disk. Must be called with pt.mu held.
func (pt *PolicyTable) save() error {
	if pt.cacheDir == "" {
		return nil
	}
	pj := policyJSON{Penalties: pt.penalties, ExtDeltas: pt.extDeltas}
	data, err := json.MarshalIndent(pj, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(pt.cacheDir, policyFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(pt.cacheDir, policyFile))
}
