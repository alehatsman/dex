package eval

import (
	"fmt"
	"math/rand"
	"sort"
)

// BootstrapParams configures the paired-bootstrap regression test. The fixed
// absolute tolerance it replaces (tol=0.02) had no statistical footing: a
// metric mean over a few hundred noisy mined queries has real run-to-run
// variance, so an arbitrary 0.02 cliff both misses small-but-real regressions
// and flags noise (the corpus gate had to disable rerank for determinism — a
// symptom). The bootstrap instead asks: is the per-query mean delta
// statistically distinguishable from zero?
//
// The knobs are standard statistical ones (confidence, resample count), not
// magnitude guesses. Seed is fixed so the gate verdict is reproducible for a
// given pair of reports — the comparator itself is deterministic.
type BootstrapParams struct {
	Resamples int     // B: number of bootstrap resamples (default 2000)
	Alpha     float64 // two-sided CI is (1-Alpha); default 0.05 → 95% CI
	Seed      int64   // fixed RNG seed so the gate is reproducible
	MinEffect float64 // practical floor: ignore drops whose point estimate is below this magnitude (default 0 = pure significance)
}

// DefaultBootstrapParams is the standard configuration: a 95% CI from 2000
// resamples, fixed seed, and no practical-effect floor (any statistically
// confident drop is a regression).
func DefaultBootstrapParams() BootstrapParams {
	return BootstrapParams{Resamples: 2000, Alpha: 0.05, Seed: 1, MinEffect: 0}
}

// BootstrapDiag reports how the current and reference query sets lined up. A
// caller that finds Paired == 0 should fall back to the fixed-tolerance
// comparator (the reports carry no per-query detail, e.g. an old report or a
// by-type bucket sub-report).
type BootstrapDiag struct {
	Paired  int // queries present (by ID) in BOTH reports — the bootstrap sample size
	OnlyNow int // query IDs in the current report but not the reference
	OnlyRef int // query IDs in the reference report but not the current
}

// metricAccessor names a gated metric and pulls its per-query value. The names
// match Report.Regressions so gate output is consistent across comparators.
type metricAccessor struct {
	name string
	get  func(QueryResult) float64
}

func gatedMetrics() []metricAccessor {
	return []metricAccessor{
		{"NDCG@k", func(q QueryResult) float64 { return q.NDCG }},
		{"Recall@k", func(q QueryResult) float64 { return q.Recall }},
		{"RecallPool@candidateK", func(q QueryResult) float64 { return q.RecallPool }},
		{"MRR", func(q QueryResult) float64 { return q.RR }},
	}
}

// BootstrapRegressions flags a metric as regressed when the paired per-query
// mean delta (now − ref) is confidently below zero: the upper bound of its
// (1-Alpha) bootstrap CI is < 0, and the point estimate drop exceeds MinEffect.
// Queries are paired by ID; unpaired queries are reported in the diag, not
// scored. When no queries pair up the result is empty with diag.Paired == 0,
// signalling the caller to fall back to the fixed-tolerance comparator.
func (rep Report) BootstrapRegressions(ref Report, p BootstrapParams) (regs []Regression, diag BootstrapDiag) {
	refByID := make(map[string]QueryResult, len(ref.Queries))
	for _, q := range ref.Queries {
		refByID[q.ID] = q
	}
	nowIDs := make(map[string]bool, len(rep.Queries))

	// Paired per-query values, kept in lockstep order across metrics.
	type pair struct{ now, was QueryResult }
	var paired []pair
	for _, q := range rep.Queries {
		nowIDs[q.ID] = true
		if was, ok := refByID[q.ID]; ok {
			paired = append(paired, pair{now: q, was: was})
		} else {
			diag.OnlyNow++
		}
	}
	for id := range refByID {
		if !nowIDs[id] {
			diag.OnlyRef++
		}
	}
	diag.Paired = len(paired)
	if diag.Paired == 0 {
		return nil, diag
	}

	for _, m := range gatedMetrics() {
		deltas := make([]float64, len(paired))
		var wasSum, nowSum float64
		for i, pr := range paired {
			now, was := m.get(pr.now), m.get(pr.was)
			deltas[i] = now - was
			nowSum += now
			wasSum += was
		}
		point, lo, hi := bootstrapMeanCI(deltas, p)
		// Regression: confident drop (whole CI below zero) past the effect floor.
		if hi < 0 && point < -p.MinEffect {
			n := float64(len(paired))
			regs = append(regs, Regression{
				Metric: m.name,
				Was:    wasSum / n,
				Now:    nowSum / n,
				CILow:  lo,
				CIHigh: hi,
				Boot:   true,
			})
		}
	}
	return regs, diag
}

// bootstrapMeanCI returns the observed mean of deltas and the lower/upper
// bounds of its (1-Alpha) two-sided percentile bootstrap CI. The RNG is seeded
// from p.Seed so the result is deterministic for a given input.
func bootstrapMeanCI(deltas []float64, p BootstrapParams) (point, lo, hi float64) {
	n := len(deltas)
	var sum float64
	for _, d := range deltas {
		sum += d
	}
	point = sum / float64(n)

	rng := rand.New(rand.NewSource(p.Seed))
	means := make([]float64, p.Resamples)
	for b := 0; b < p.Resamples; b++ {
		var s float64
		for i := 0; i < n; i++ {
			s += deltas[rng.Intn(n)]
		}
		means[b] = s / float64(n)
	}
	sort.Float64s(means)
	lo = percentile(means, p.Alpha/2)
	hi = percentile(means, 1-p.Alpha/2)
	return point, lo, hi
}

// percentile returns the q-quantile (0..1) of a sorted slice by nearest-rank.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(q*float64(len(sorted)-1) + 0.5)
	return sorted[idx]
}

// bootstrapNote renders a one-line summary for gate output.
func (r Regression) bootstrapNote() string {
	if !r.Boot {
		return ""
	}
	return fmt.Sprintf(" [95%% CI %.3f..%.3f]", r.CILow, r.CIHigh)
}
