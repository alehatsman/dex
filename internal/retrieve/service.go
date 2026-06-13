package retrieve

import (
	"context"
	"errors"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
)

// Service is the query-time retrieval engine. It holds the backends the
// search lanes need (today just an embedder; the chat/expand backends and
// caches join as later cuts move in) and exposes the lanes as methods. It
// is constructed per request by the transport from the server's wired
// clients — cheap, stateless beyond its backends.
type Service struct {
	// Embed is the embedding backend for the semantic lane. Optional: nil
	// runs the lane BM25-only (the lean DEX_EMBED_ENGINE=none profile).
	Embed embed.Embedder
}

// SemHit is one semantic-lane result in neutral (transport-free) form —
// the raw ranked chunk the lane produces, before the transport overlays
// inline content, handles, and seen-turn dedup.
type SemHit struct {
	Path      string
	StartLine int
	EndLine   int
	Kind      string
	Score     float32
	Reason    string
}

// SymHit is one symbol-lane result in neutral form. It carries the raw
// call-graph centrality columns rather than a formatted role string so
// the transport (which owns the role-display vocabulary, shared with the
// search/symbol/graph tools) formats the Role at the edge.
type SymHit struct {
	QualifiedName string
	// Name is the raw symbol name as stored (may be empty); it is the
	// first argument formatRole consumes at the transport edge.
	Name            string
	Path            string
	StartLine       int
	EndLine         int
	Kind            string
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	Betweenness     float64
}

// SymbolLane runs FindSymbol for each detected identifier and returns
// deduplicated symbol hits plus the set of file paths the lane touched
// (used by the suggested_reads ranker). At most k hits are returned. The
// transport maps these to wire SymbolHits — formatting the Role and
// applying the test/doc/fixture demotion that lands the prose directive
// on real implementation code.
func (svc Service) SymbolLane(ctx context.Context, st store.Searcher, cand IntentCandidates, k int) ([]SymHit, map[string]struct{}) {
	if len(cand.Identifiers) == 0 {
		return nil, nil
	}
	paths := map[string]struct{}{}
	seen := map[string]struct{}{}
	var out []SymHit
	for _, id := range cand.Identifiers {
		// FindSymbol expects the bare name; strip a "(*T)." prefix.
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
			out = append(out, SymHit{
				QualifiedName:   qual,
				Name:            h.Name,
				Path:            h.Path,
				StartLine:       h.StartLine,
				EndLine:         h.EndLine,
				Kind:            h.Kind,
				InDegree:        h.InDegree,
				OutDegree:       h.OutDegree,
				CrossPkgCallers: h.CrossPkgCallers,
				Betweenness:     h.Betweenness,
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
	return out, paths
}

// SemanticLane embeds embedText and runs Search with ftsText as the
// BM25/FTS leg of the RRF fusion. The two are usually identical (the raw
// question); query-side expansion (#252) lets them diverge — extra
// keywords widen the lexical leg for free, while a HyDE passage shifts
// only the embedded vector. Returns (hits, embedUnreachable). When
// embedUnreachable is true hits is nil and the caller should surface the
// failure.
func (svc Service) SemanticLane(ctx context.Context, st store.Searcher, embedText, ftsText string, k int) ([]SemHit, bool) {
	// queryVec stays nil in the lean profile (DEX_EMBED_ENGINE=none, no
	// embedder wired). Search then runs BM25-only through the fusion path
	// — the semantic leg contributes nothing — so ask still answers on the
	// lexical lane (plus the symbol + graph lanes upstream). This is the
	// documented lean degradation; only a *downed* embedder
	// (ErrUnreachable) is reported as unreachable so the caller can
	// surface the failure.
	var queryVec []float32
	if em := svc.Embed; em != nil {
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
		// In hybrid mode, Hit.Score is raw cosine — zero for hits that
		// came in via BM25 only (the FTS leg of the RRF fusion).
		// Surfacing 0 here misleads the agent into thinking it's looking
		// at irrelevant content. Fall back to the RRF score so every
		// returned hit has a positive ranking signal. Scales differ
		// (cosine ~0-1, RRF ~0-0.03) but ordering within the list is what
		// matters.
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
