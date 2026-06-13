package mcp

import (
	"context"
	"sort"

	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
)

// ─── lanes ────────────────────────────────────────────────────────────────
//
// The lane bodies (identifier resolution + symbol search, query
// embedding + hybrid search) live in retrieve.Service. These wrappers
// adapt the neutral lane output to the wire types and apply the two
// transport-edge concerns: the call-graph Role string (formatRole, whose
// vocabulary is shared with the search/symbol/graph tools) and the
// test/doc/fixture demotion that keeps the prose directive on real
// implementation code.

func (s *Server) runSymbolLane(ctx context.Context, st store.Searcher, cand retrieve.IntentCandidates, k int) ([]SymbolHit, map[string]struct{}) {
	svc := retrieve.Service{Embed: s.EmbedClient}
	raw, paths := svc.SymbolLane(ctx, st, cand, k)
	if raw == nil {
		return nil, paths
	}
	out := make([]SymbolHit, 0, len(raw))
	for _, h := range raw {
		out = append(out, SymbolHit{
			QualifiedName: h.QualifiedName,
			Path:          h.Path,
			StartLine:     h.StartLine,
			EndLine:       h.EndLine,
			Kind:          h.Kind,
			Role:          formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
		})
	}
	// Demote test/doc/build/fixture paths so the prose directive (which
	// points at the first symbol) lands on real implementation. FindSymbol
	// returns rows sorted by (path, start_line), which alphabetically lifts
	// `internal/graph/testdata/...` above the real `internal/store/...` for
	// shared names like `Store`.
	sort.SliceStable(out, func(i, j int) bool {
		return !isNonImplPath(out[i].Path) && isNonImplPath(out[j].Path)
	})
	return out, paths
}

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
