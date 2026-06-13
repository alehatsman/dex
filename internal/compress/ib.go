package compress

import (
	"math"
	"sort"
	"strings"
)

// charBigramLM maps a 2-char bigram to the distribution of following characters
// observed across the training text (document-level char-trigram model).
type charBigramLM map[string][256]int

// CompressIB compresses text to approximately targetRatio of its original
// token count using AdmTree-style segment scoring with three-tier budget
// allocation.
//
// Algorithm:
//  1. Split text into 20-line segments.
//  2. Score each segment: novelty(seg) × exp(−0.5 × entropy(seg)).
//     novelty = average inverse segment-frequency of the segment's char-trigrams
//     (rare trigrams → high novelty → perplexity proxy).
//     entropy = average normalized word entropy of non-blank lines.
//  3. Binary-search the tier-1 cutoff score to hit targetRatio:
//     tier 1 (score ≥ cutoff)    → preserve verbatim
//     tier 2 (score ≥ cutoff/2)  → drop low-entropy lines within the segment
//     tier 3 (score <  cutoff/2) → drop segment body; keep blank lines only
//
// This replaces the original uniform single-threshold binary search. It
// preferentially preserves dense, novel segments (logic, type definitions)
// while aggressively trimming repetitive boilerplate (import blocks, repeated
// log lines, auto-generated tables).
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

	origTokens := countTokens(text)
	if origTokens == 0 {
		return text
	}

	segs := makeIBSegments(lines, 20)
	lm := buildCharLM(text)
	scores := make([]float64, len(segs))
	for i, seg := range segs {
		scores[i] = admTreeScore(seg, lm)
	}

	result := ibAllocate(segs, scores, targetRatio, origTokens)
	if len(result) >= len(text) {
		return text
	}
	return result
}

// ibSegment is a contiguous window of lines with a pre-counted token budget.
type ibSegment struct {
	lines  []string
	tokens int
}

func makeIBSegments(lines []string, windowSize int) []ibSegment {
	var segs []ibSegment
	for i := 0; i < len(lines); i += windowSize {
		end := i + windowSize
		if end > len(lines) {
			end = len(lines)
		}
		sl := lines[i:end]
		segs = append(segs, ibSegment{
			lines:  sl,
			tokens: countTokens(strings.Join(sl, "\n")),
		})
	}
	return segs
}

// buildCharLM constructs a character-level bigram→nextchar language model from
// the full document text. This is the PPL reference: segments whose character
// transitions are well-predicted by this model have low cross-entropy (low PPL)
// and are considered redundant/boilerplate. Segments with surprising transitions
// (high CE) carry novel information and should be preserved.
func buildCharLM(text string) charBigramLM {
	lm := make(charBigramLM, 512)
	b := []byte(text)
	for i := 0; i+2 < len(b); i++ {
		bg := string(b[i : i+2])
		entry := lm[bg]
		entry[b[i+2]]++
		lm[bg] = entry
	}
	return lm
}

// segmentCE computes the mean character-level cross-entropy of text under lm
// (bits per character). Unseen bigrams contribute log2(256)=8 bits (max surprise).
func segmentCE(text string, lm charBigramLM) float64 {
	b := []byte(text)
	if len(b) < 3 {
		return 4.0 // neutral mid-range for very short segments
	}
	var total float64
	count := 0
	for i := 0; i+2 < len(b); i++ {
		next := b[i+2]
		dist, ok := lm[string(b[i:i+2])]
		if !ok {
			total += 8.0
		} else {
			var sum int
			for _, c := range dist {
				sum += c
			}
			p := float64(dist[next]) / float64(sum)
			if p > 0 {
				total -= math.Log2(p)
			} else {
				total += 8.0
			}
		}
		count++
	}
	if count == 0 {
		return 4.0
	}
	return total / float64(count)
}

// admTreeScore returns Score(Xi) = CE(Xi) × exp(−λ × entropy(Xi)).
//
// CE is the character-level cross-entropy of the segment under the document's
// bigram LM — a proxy for perplexity. High CE means the segment's character
// patterns are hard to predict from the rest of the document (novel content).
// entropy is the mean normalized word entropy of non-blank lines; high entropy
// indicates noisy/boilerplate content (e.g. import blocks with unique pkg names).
// λ=0.5 dampens the entropy penalty so that dense-but-noisy segments are
// deprioritized relative to clean logic, without being dropped entirely.
func admTreeScore(seg ibSegment, lm charBigramLM) float64 {
	ce := segmentCE(strings.Join(seg.lines, "\n"), lm)

	var entropy float64
	nonBlank := 0
	for _, line := range seg.lines {
		if strings.TrimSpace(line) != "" {
			entropy += normalizedWordEntropy(line)
			nonBlank++
		}
	}
	if nonBlank > 0 {
		entropy /= float64(nonBlank)
	}

	const lambda = 0.5
	return ce * math.Exp(-lambda*entropy)
}

// ibAllocate reconstructs compressed text by binary-searching the tier-1 score
// cutoff to hit targetRatio.
func ibAllocate(segs []ibSegment, scores []float64, targetRatio float64, origTokens int) string {
	targetToks := int(float64(origTokens) * targetRatio)

	// Sorted score values provide the search domain.
	sorted := make([]float64, len(scores))
	copy(sorted, scores)
	sort.Float64s(sorted)

	applyTiers := func(cutoff float64) string {
		var sb strings.Builder
		for i, seg := range segs {
			switch {
			case scores[i] >= cutoff:
				// Tier 1: preserve verbatim.
				sb.WriteString(strings.Join(seg.lines, "\n"))
				sb.WriteByte('\n')
			case scores[i] >= cutoff/2:
				// Tier 2: drop lines scoring below the moderate word-entropy
				// threshold; blank lines always kept.
				for _, line := range seg.lines {
					if strings.TrimSpace(line) == "" || normalizedWordEntropy(line) >= 0.4 {
						sb.WriteString(line)
						sb.WriteByte('\n')
					}
				}
			default:
				// Tier 3: segment body dropped; blank lines kept as structural markers.
				for _, line := range seg.lines {
					if strings.TrimSpace(line) == "" {
						sb.WriteString(line)
						sb.WriteByte('\n')
					}
				}
			}
		}
		return sb.String()
	}

	lo, hi := sorted[0], sorted[len(sorted)-1]
	var bestResult string
	bestDelta := math.MaxFloat64

	for range 26 {
		mid := (lo + hi) / 2
		candidate := applyTiers(mid)
		toks := countTokens(candidate)
		delta := float64(toks - targetToks)

		if delta >= 0 && delta < bestDelta {
			bestResult = candidate
			bestDelta = delta
		}

		if toks > targetToks {
			lo = mid // too many tokens: raise cutoff
		} else {
			hi = mid // too few tokens: lower cutoff
		}
	}

	if bestResult == "" {
		bestResult = applyTiers(lo)
	}
	return bestResult
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
