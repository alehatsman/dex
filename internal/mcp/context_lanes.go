package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
)

// ─── lanes ────────────────────────────────────────────────────────────────

// runSymbolLane runs search_symbol for each detected identifier and
// returns deduplicated symbol hits plus a set of file paths the lane
// touched (used by pickSuggestedReads). At most `k` hits returned.
func (s *Server) runSymbolLane(ctx context.Context, st store.Searcher, cand intentCandidates, k int) ([]SymbolHit, map[string]struct{}) {
	if len(cand.identifiers) == 0 {
		return nil, nil
	}
	paths := map[string]struct{}{}
	seen := map[string]struct{}{}
	var out []SymbolHit
	for _, id := range cand.identifiers {
		// search_symbol expects the bare name; strip a "(*T)." prefix.
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		hits, err := st.FindSymbol(ctx, bare, k)
		if err != nil {
			continue
		}
		for _, h := range hits {
			key := h.Path + ":" + h.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			qual := h.Name
			if h.Name == "" {
				qual = bare
			}
			out = append(out, SymbolHit{
				QualifiedName: qual,
				Path:          h.Path,
				StartLine:     h.StartLine,
				EndLine:       h.EndLine,
				Kind:          h.Kind,
				Role:          formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			})
			paths[h.Path] = struct{}{}
			if len(out) >= k {
				break
			}
		}
		if len(out) >= k {
			break
		}
	}
	// Demote test/doc/build/fixture paths so the prose directive
	// (which points at the first symbol) lands on real implementation.
	// FindSymbol returns rows sorted by (path, start_line), which
	// alphabetically lifts `internal/graph/testdata/...` above the
	// real `internal/store/...` for shared names like `Store`.
	sort.SliceStable(out, func(i, j int) bool {
		return !isNonImplPath(out[i].Path) && isNonImplPath(out[j].Path)
	})
	return out, paths
}

// runSemanticLane embeds embedText and runs Search with ftsText as the
// BM25/FTS leg of the RRF fusion. The two are usually identical (the raw
// question); query-side expansion (#252) lets them diverge — extra keywords
// widen the lexical leg for free, while a HyDE passage shifts only the
// embedded vector. Returns (hits, embedUnreachable). When embedUnreachable
// is true hits is nil and the caller should surface the failure.
func (s *Server) runSemanticLane(ctx context.Context, st store.Searcher, embedText, ftsText string, k int) ([]SemHit, bool) {
	// queryVec stays nil in the lean profile (DEX_EMBED_ENGINE=none, no embedder
	// wired). Search then runs BM25-only through the fusion path — the semantic
	// leg contributes nothing — so ask still answers on the lexical lane (plus
	// the symbol + graph lanes upstream). This is the documented lean
	// degradation; only a *downed* embedder (ErrUnreachable) is reported as
	// unreachable so the caller can surface the failure.
	var queryVec []float32
	if em := s.EmbedClient; em != nil {
		vecs, err := em.Embed(ctx, []string{embedText})
		if err != nil {
			if errors.Is(err, embed.ErrUnreachable) {
				return nil, true
			}
			return nil, false
		}
		queryVec = vecs[0]
	}
	hits, err := st.Search(ctx, queryVec, ftsText, k)
	if err != nil {
		return nil, false
	}
	out := make([]SemHit, 0, len(hits))
	for _, h := range hits {
		// In hybrid mode, Hit.Score is raw cosine — zero for hits
		// that came in via BM25 only (the FTS leg of the RRF fusion).
		// Surfacing 0 here misleads the agent into thinking it's
		// looking at irrelevant content. Fall back to the RRF
		// score so every returned hit has a positive ranking signal.
		// Scales differ (cosine ~0-1, RRF ~0-0.03) but ordering
		// within the list is what matters.
		score := h.Score
		if score == 0 && h.RRFScore > 0 {
			score = h.RRFScore
		}
		out = append(out, SemHit{
			Path:      h.Path,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Kind:      h.Kind,
			Score:     score,
			Reason:    h.Name,
		})
	}
	return out, false
}

// Path-classification helpers (isTestPath, isNonImplPath, pathTags)
// live in path_tags.go — one rule table for every demotion the
// suggested_reads ranker applies.

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

// isReadableRange reports whether a SemHit points at a concrete file
// slice the agent can actually `Read`. Rollup chunks (package_summary,
// repo_summary) have Path set to a directory; they're useful context
// in semantic_hits but should not land in suggested_reads where
// "lines 0-0" reads as a Read directive the agent can't execute.
func isReadableRange(h SemHit) bool {
	switch h.Kind {
	case "package_summary", "repo_summary":
		return false
	}
	return true
}
