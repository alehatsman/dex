package retrieve

import (
	"sort"

	"github.com/alehatsman/dex/internal/store"
)

// FuseWithSymbols merges a semantic hit list with a symbol hit list via
// Reciprocal Rank Fusion (k=60) and returns the top-n results. The
// dedup key is (path, start_line). Semantic hits already carry Score /
// BM25Score / RRFScore from the store; symbol-only hits get Score=1.0
// (exact-match signal). The new RRFScore field reflects the cross-lane
// fused rank for all returned hits.
//
// Like the graph lane, both legs are scored from rank position only — the
// incoming Hit.Score magnitude is discarded — so this stage is fusion-mode
// independent (FusionRRF vs FusionLinear changes only the semantic ORDER, not
// the symbol lane's relative weight).
func FuseWithSymbols(semantic, symbol []store.Hit, n int) []store.Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(semantic)+len(symbol))
	byKey := make(map[hitKey]store.Hit, len(semantic)+len(symbol))

	for i, h := range semantic {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
	}
	for i, h := range symbol {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		if _, exists := byKey[hk]; !exists {
			h.Score = 1.0 // exact name match
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}

	out := make([]store.Hit, 0, len(all))
	for _, r := range all {
		h := byKey[r.key]
		h.RRFScore = r.score
		out = append(out, h)
	}
	return out
}
