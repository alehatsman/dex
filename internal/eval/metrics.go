// Package eval implements an offline code-retrieval evaluation harness.
//
// It scores the live dex Search path against a golden set of labeled
// (query → relevant files) pairs derived from a repo's own git history,
// reporting the standard IR metrics NDCG@k, Recall@k and MRR. The golden
// set is reproducible from git log (see golden.go); the runner (runner.go)
// queries an existing project index read-only.
package eval

import "math"

// NDCG computes the normalized discounted cumulative gain at depth k for a
// ranked list of file paths against a set of relevant files. Gains are
// binary (1 if the ranked item is relevant, else 0). Returns 0 when there
// are no relevant items (IDCG == 0).
func NDCG(ranked []string, relevant map[string]bool, k int) float64 {
	if k <= 0 || len(relevant) == 0 {
		return 0
	}
	dcg := 0.0
	for i := 0; i < k && i < len(ranked); i++ {
		if relevant[ranked[i]] {
			dcg += 1.0 / math.Log2(float64(i)+2.0)
		}
	}
	// Ideal DCG: all relevant items packed at the top, capped at k.
	ideal := len(relevant)
	if ideal > k {
		ideal = k
	}
	idcg := 0.0
	for i := 0; i < ideal; i++ {
		idcg += 1.0 / math.Log2(float64(i)+2.0)
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// RecallAtK is the fraction of relevant files that appear in the top-k of
// the ranked list. Returns 0 when there are no relevant items.
func RecallAtK(ranked []string, relevant map[string]bool, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	hits := 0
	for i := 0; i < k && i < len(ranked); i++ {
		if relevant[ranked[i]] {
			hits++
		}
	}
	return float64(hits) / float64(len(relevant))
}

// MRR is the reciprocal rank of the first relevant file in the ranked list
// (1-indexed). Returns 0 when no relevant file appears anywhere in the list.
func MRR(ranked []string, relevant map[string]bool) float64 {
	for i, p := range ranked {
		if relevant[p] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}
