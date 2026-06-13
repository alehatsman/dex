package locomo

import (
	"strings"
	"unicode"
)

// TokenF1 computes the token-level F1 between hypothesis and reference.
// Tokens are whitespace-split, lowercased, punctuation-stripped words.
// Matches the qa_f1 scorer from lean-ctx / SQuAD evaluation.
func TokenF1(hyp, ref string) float64 {
	hTokens := tokenize(hyp)
	rTokens := tokenize(ref)
	if len(hTokens) == 0 && len(rTokens) == 0 {
		return 1.0
	}
	if len(hTokens) == 0 || len(rTokens) == 0 {
		return 0.0
	}
	// Count token overlap.
	rCounts := make(map[string]int, len(rTokens))
	for _, t := range rTokens {
		rCounts[t]++
	}
	common := 0
	for _, t := range hTokens {
		if rCounts[t] > 0 {
			common++
			rCounts[t]--
		}
	}
	if common == 0 {
		return 0.0
	}
	precision := float64(common) / float64(len(hTokens))
	recall := float64(common) / float64(len(rTokens))
	return 2 * precision * recall / (precision + recall)
}

// ExactMatch returns true when the normalized hypothesis equals the normalized
// reference. Normalization: lowercase, collapse whitespace, strip punctuation.
func ExactMatch(hyp, ref string) bool {
	return normalize(hyp) == normalize(ref)
}

// Contains returns true when the normalized reference is a substring of the
// normalized hypothesis (answer-containment / recall@k metric).
func Contains(hyp, ref string) bool {
	return strings.Contains(normalize(hyp), normalize(ref))
}

func tokenize(s string) []string {
	s = normalize(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

func normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
