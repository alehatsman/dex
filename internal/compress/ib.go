package compress

import (
	"math"
	"strings"
)

// CompressIB compresses text to approximately targetRatio of its original
// token count using information-bottleneck binary search.
//
// Algorithm (26-iteration binary search):
//  1. Score each line by normalizedWordEntropy — a [0, 1] measure of how
//     information-dense the line is (0 = repetitive/boilerplate, 1 = unique).
//  2. Binary-search for the entropy threshold that, when lines below it are
//     dropped, yields output/input token ratio ≈ targetRatio.
//  3. Return the candidate closest to targetRatio without going below it
//     (prefer slightly longer over silently over-compressed).
//
// Blank lines are always kept (structural markers). Lines with fewer than
// 2 distinct word tokens score 0 and are dropped at any positive threshold.
//
// targetRatio must be in (0, 1). Values outside this range return text unchanged.
// Inputs shorter than 5 lines are returned unchanged.
func CompressIB(text string, targetRatio float64) string {
	if targetRatio <= 0 || targetRatio >= 1 {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) < 5 {
		return text
	}

	scores := make([]float64, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			scores[i] = -1 // sentinel: always keep blank lines
		} else {
			scores[i] = normalizedWordEntropy(line)
		}
	}

	origTokens := countTokens(text)
	if origTokens == 0 {
		return text
	}

	// Binary search: lo = lower threshold bound, hi = upper bound.
	// Higher threshold → more lines dropped → lower output ratio.
	lo, hi := 0.0, 1.0
	var bestLines []string
	bestDelta := math.MaxFloat64

	for range 26 {
		mid := (lo + hi) / 2
		candidate := keepAboveThreshold(lines, scores, mid)
		outTokens := countTokens(strings.Join(candidate, "\n"))
		ratio := float64(outTokens) / float64(origTokens)

		delta := ratio - targetRatio
		if delta >= 0 && delta < bestDelta {
			// Candidate is at or above targetRatio and closer than best so far.
			bestLines = candidate
			bestDelta = delta
		}

		if ratio > targetRatio {
			lo = mid // still too long: raise threshold
		} else {
			hi = mid // over-compressed: lower threshold
		}
	}

	if bestLines == nil {
		// Every threshold over-compressed; return least-compressed candidate.
		bestLines = keepAboveThreshold(lines, scores, lo)
	}

	result := strings.Join(bestLines, "\n")
	// Never return something longer than the original (pathological edge case).
	if len(result) >= len(text) {
		return text
	}
	return result
}

// normalizedWordEntropy computes H(words) / log2(|unique_words|) for a line,
// yielding a [0, 1] information-density score.
//
//   - 0: all words identical, or fewer than 2 distinct words (boilerplate)
//   - 1: all words distinct (maximum information density)
//
// Uses lowercase word tokens to treat case variants as the same token.
func normalizedWordEntropy(line string) float64 {
	words := strings.Fields(line)
	if len(words) == 0 {
		return 0
	}

	freq := make(map[string]int, len(words))
	for _, w := range words {
		freq[strings.ToLower(w)]++
	}
	unique := len(freq)
	if unique <= 1 {
		return 0 // single/uniform content → minimum entropy
	}

	n := float64(len(words))
	var h float64
	for _, f := range freq {
		p := float64(f) / n
		h -= p * math.Log2(p)
	}
	return h / math.Log2(float64(unique))
}

// keepAboveThreshold returns lines whose score is above threshold, always
// including blank lines (score == -1 sentinel).
func keepAboveThreshold(lines []string, scores []float64, threshold float64) []string {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if scores[i] < 0 || scores[i] >= threshold {
			out = append(out, line)
		}
	}
	return out
}
