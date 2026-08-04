package mcp

import (
	"context"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
)

// ─── lanes ────────────────────────────────────────────────────────────────
//
// The lane bodies (query embedding + hybrid search) live in
// retrieve.Service; runSemanticLane adapts the neutral output to the wire
// SemHit. The symbol lane, its shared Role vocabulary and the test/doc/
// fixture demotion now compose in retrieve.Assembler (#95a), which the
// context router drives directly.

func (s *Server) runSemanticLane(ctx context.Context, st store.Searcher, embedText, ftsText string, k int) ([]SemHit, bool) {
	svc := retrieve.Service{Embed: s.EmbedClient}
	raw, embedFailed := svc.SemanticLane(ctx, st, embedText, ftsText, k)
	if raw == nil {
		return nil, embedFailed
	}
	out := make([]SemHit, 0, len(raw))
	for _, h := range raw {
		out = append(out, SemHit{
			Path:      h.Path,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Kind:      h.Kind,
			Score:     h.Score,
			Reason:    h.Reason,
			Lanes:     h.Lanes.Names(),
		})
	}
	return out, embedFailed
}

// maxSemanticScore returns the highest Score across all semantic
// hits. semantic_hits isn't strictly score-sorted (summary merging
// and rerank-driven re-ordering permute it), so using [0] for the
// "weak match" decision mis-classifies strong responses whenever a
// low-score symbol-driven entry gets promoted to the front.
func maxSemanticScore(hits []SemHit) float32 {
	var top float32
	for _, h := range hits {
		if h.Score > top {
			top = h.Score
		}
	}
	return top
}
