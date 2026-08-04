package retrieve

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

// SearchAssembler composes the semantic-search ranking pipeline into a ranked
// []store.Hit — the domain core of the `search` tool (#111), the search-verb
// twin of Assembler. It holds the retrieval Service (embedder + cross-encoder)
// and runs the fuse→symbol→spread→rerank→ecs→multi-scale sequence. The
// transport (mcp) owns everything around it: the query embedding (so it can
// surface ErrUnreachable + endpoint), the index/staleness checks, throttle,
// loop detection, SLO, language/path_glob filtering, handle stamping and the
// wire projection.
type SearchAssembler struct {
	// Service is the query-time retrieval engine — supplies the cross-encoder
	// rerank (RerankFused) and its shared score cache.
	Service Service
}

// SearchRequest carries the resolved per-request inputs the ranking core needs.
// Vec is the query embedding (nil in the lean BM25-only profile); the transport
// embeds and owns the ErrUnreachable surfacing. MultiScale is the
// transport-owned TF-IDF path filter (the server holds the multi-scale index
// cache) — injected as a hook, per the Assembler policy-injection pattern; nil
// skips it.
type SearchRequest struct {
	Query       string
	Vec         []float32
	CandidateK  int
	SessionTask string
	MultiScale  func(hits []store.Hit) []store.Hit
}

// Assemble runs the search ranking pipeline over the candidate pool. It is
// behavior-neutral with the former inline server_search sequence: same lane
// order (fused → symbol RRF → spreading activation → cross-encoder rerank →
// ECS rerank → multi-scale filter). It returns the ranked hits through the
// multi-scale filter; the transport applies the language/path_glob filter and
// the k-truncation afterward. Stage errors are prefixed here so the transport's
// hint text stays byte-identical to the pre-seam handler.
func (a SearchAssembler) Assemble(ctx context.Context, st *store.Store, req SearchRequest) ([]store.Hit, error) {
	hits, err := st.SearchFused(ctx, req.Vec, req.Query, req.CandidateK)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	// Symbol leg: extract identifier tokens from the query, look them up by
	// exact name, and RRF-fuse with the semantic results. Runs in the same
	// request with no extra embedding round-trip — FindSymbol is a pure SQL
	// index scan.
	hits = collectSearchSymbolLeg(ctx, st, req.Query, hits, req.CandidateK)

	// Graph-proximity lane: spreading activation from session-recent files and
	// the current semantic hits. Silently skips when no session exists or the
	// graph hasn't been built — never fails the search.
	hits = st.FuseSpreadingActivation(ctx, hits, req.Vec, req.CandidateK)

	hits, err = a.Service.RerankFused(ctx, req.Query, hits, req.CandidateK)
	if err != nil {
		return nil, fmt.Errorf("rerank: %w", err)
	}
	hits = RerankECS(hits, req.SessionTask)

	// Multi-scale TF-IDF path filter — runs after the full fusion+rerank
	// pipeline. Applying it earlier would prune the candidate pool before
	// symbol fusion and spreading activation, starving both lanes.
	if req.MultiScale != nil {
		hits = req.MultiScale(hits)
	}
	return hits, nil
}

// collectSearchSymbolLeg fuses exact-name symbol hits into the semantic result
// set via RRF. A no-op when the query contains no identifier-shaped tokens.
func collectSearchSymbolLeg(ctx context.Context, st *store.Store, query string, hits []store.Hit, candidateK int) []store.Hit {
	idents := ExtractIdentifiers(query)
	if len(idents) == 0 {
		return hits
	}
	symPool := candidateK * 3
	if symPool < 15 {
		symPool = 15
	}
	if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
		hits = FuseWithSymbols(hits, symHits, candidateK)
	}
	return hits
}

// collectSymbolHits runs FindSymbol for each identifier and returns a
// deduplicated hit list (keyed by path+start_line), in the order they
// surfaced across all identifier queries.
func collectSymbolHits(ctx context.Context, st *store.Store, idents []string, pool int) []store.Hit {
	seen := map[string]struct{}{}
	var out []store.Hit
	for _, id := range idents {
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		hits, err := st.FindSymbol(ctx, bare, pool)
		if err != nil {
			continue
		}
		for _, h := range hits {
			key := h.Path + ":" + strconv.Itoa(h.StartLine)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, h)
			if len(out) >= pool {
				return out
			}
		}
	}
	return out
}
