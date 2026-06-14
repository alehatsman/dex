package mcp

import "github.com/alehatsman/dex/internal/retrieve"

// sortSuggestedReadsByAttention returns a new slice with the highest-scoring
// chunks first (L-curve: model attends strongest to position 0).
// The original slice is not modified. Stable sort preserves the relative
// order of equal-scoring entries.
//
// The importance scoring is owned by retrieve (retrieve.ChunkImportance);
// this adapter applies it to the wire SuggestedRead type at evidence-build
// time.
func sortSuggestedReadsByAttention(reads []SuggestedRead) []SuggestedRead {
	if len(reads) <= 1 {
		return reads
	}
	scored := make([]struct {
		r     SuggestedRead
		score float64
	}, len(reads))
	for i, r := range reads {
		scored[i] = struct {
			r     SuggestedRead
			score float64
		}{r, retrieve.ChunkImportance(r.Content)}
	}
	// Insertion sort (stable, small n).
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].score > scored[j-1].score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	out := make([]SuggestedRead, len(scored))
	for i, s := range scored {
		out[i] = s.r
	}
	return out
}
