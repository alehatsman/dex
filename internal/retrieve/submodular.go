package retrieve

// Submodular max-coverage selection (#687, epic #683). The assemble intent
// turns `ask` into a context-assembler: instead of inlining symbol bodies in
// rank order, it picks the NON-REDUNDANT subset that covers the most query
// keywords per byte spent. Greedy max-coverage gives a (1 - 1/e) ≈ 0.63
// approximation of the optimal keyword coverage for the budget — and, crucially,
// it skips a symbol that only re-covers keywords an earlier pick already
// covered, so the bundle isn't three variations of the same match.

// Coverable is one candidate for selection: the keywords it covers and the cost
// (in whatever unit the caller budgets — lines or bytes) of including it.
type Coverable struct {
	Keys []string // keywords this item covers (caller normalizes case)
	Cost int      // inclusion cost; items with Cost <= 0 are skipped
}

// SelectMaxCoverage runs greedy submodular max-coverage: repeatedly take the
// item with the highest (newly-covered keywords / cost) ratio until the budget
// is exhausted or no remaining item covers a new keyword. Returns the selected
// item indices in pick order.
//
// A budget <= 0 is treated as unbounded — every item that contributes new
// coverage is ordered (the caller then enforces its own byte cap downstream).
// Items covering zero keywords are never selected.
func SelectMaxCoverage(items []Coverable, budget int) []int {
	covered := make(map[string]bool)
	used := make([]bool, len(items))
	order := make([]int, 0, len(items))
	remaining := budget
	unbounded := budget <= 0

	for {
		best := -1
		var bestRatio float64
		for i, it := range items {
			if used[i] || it.Cost <= 0 {
				continue
			}
			if !unbounded && it.Cost > remaining {
				continue
			}
			newCov := 0
			for _, k := range it.Keys {
				if !covered[k] {
					newCov++
				}
			}
			if newCov == 0 {
				continue
			}
			ratio := float64(newCov) / float64(it.Cost)
			if best == -1 || ratio > bestRatio {
				best, bestRatio = i, ratio
			}
		}
		if best == -1 {
			break
		}
		used[best] = true
		order = append(order, best)
		if !unbounded {
			remaining -= items[best].Cost
		}
		for _, k := range items[best].Keys {
			covered[k] = true
		}
	}
	return order
}
