package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"database/sql"
)

// Hit is one search result.
type Hit struct {
	Path      string
	Kind      string
	Name      string
	StartLine int
	EndLine   int
	Content   string

	// Score is the cosine similarity in [-1, 1] (1.0 == identical
	// direction). Always populated, even for hits that surfaced via
	// the BM25 path — useful as a familiar "is this close?" number
	// for humans and for downstream filtering.
	Score float32

	// BM25Score is the FTS5 bm25() rank when the hit surfaced through
	// the lexical path. SQLite returns these as small negative
	// numbers (more negative = better); we negate so larger = better.
	// Zero when the hit didn't match the BM25 query at all.
	BM25Score float32

	// RRFScore is the fused rank used for ordering when hybrid search
	// is active: 1/(60+sem_rank) + 1/(60+bm25_rank). Zero when search
	// ran semantic-only (empty query text or DEX_DISABLE_BM25=1).
	RRFScore float32

	// RerankScore is the cross-encoder relevance score in [0, 1] for
	// the (query, chunk) pair. Zero when rerank didn't run (no client
	// wired, pool ≤ k, or endpoint unreachable). Larger = more relevant.
	RerankScore float32

	// SortScore is the effective value the hits are ordered by after the
	// local rerank pass (noise penalty, definition/coherence boosts, MMR).
	// It is the *authoritative* sort key when the cross-encoder did not
	// run, since those passes reorder relative to RRFScore — so display
	// this (not Score/RRFScore) to keep the visible ranking monotonic.
	// Zero when local rerank was skipped (cross-encoder path, where
	// RerankScore is the sort key instead).
	SortScore float32

	// Centrality fields — populated from graph_nodes via the
	// chunk_id join when the symbol has a corresponding graph node.
	// Zero when no graph node exists (the file is in an unindexed
	// language, the chunk isn't a function/method, or the graph hasn't
	// been built yet). Callers use these to sort and to compose the
	// role-hint shown to agents.
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	PageRank        float64
	Betweenness     float64
	// Signature is the stored declaration header from graph_nodes.signature,
	// populated by the Go extractor via go/types. Empty for non-Go symbols or
	// when the graph hasn't been built. Callers should prefer this over reading
	// the source file when non-empty.
	Signature string

	// Lanes records which retrieval lanes surfaced this hit (vector, bm25,
	// symbol, graph). It is pure provenance — never read by ranking — so an
	// agent can weight a hit that several independent lanes agreed on above a
	// single-lane hit, instead of trusting top-K position uniformly (#707).
	// The set is unioned across the fusion stages: dense/BM25 at searchRaw,
	// symbol at FuseWithSymbols, graph at fuseWithGraphNeighbors.
	Lanes LaneSet
}

// Lane is one retrieval lane that can contribute a hit. A hit may surface
// through several lanes at once; the union is recorded on Hit.Lanes.
type Lane uint8

const (
	LaneVector Lane = 1 << iota // dense cosine KNN
	LaneBM25                    // lexical FTS5/BM25
	LaneSymbol                  // exact symbol-name lookup
	LaneGraph                   // graph-proximity expansion
)

// LaneSet is a bitset of the lanes a hit surfaced through. The zero value is
// the empty set (no lane recorded — e.g. a hit assembled outside the fusion
// path).
type LaneSet uint8

// With returns the set with l added (union of a single lane).
func (s LaneSet) With(l Lane) LaneSet { return s | LaneSet(l) }

// Has reports whether l is in the set.
func (s LaneSet) Has(l Lane) bool { return s&LaneSet(l) != 0 }

// Names renders the set as lane tags in a stable order (vector, bm25, symbol,
// graph). Returns nil for the empty set so callers can omit an empty `lanes`
// field on the wire.
func (s LaneSet) Names() []string {
	if s == 0 {
		return nil
	}
	var out []string
	if s.Has(LaneVector) {
		out = append(out, "vector")
	}
	if s.Has(LaneBM25) {
		out = append(out, "bm25")
	}
	if s.Has(LaneSymbol) {
		out = append(out, "symbol")
	}
	if s.Has(LaneGraph) {
		out = append(out, "graph")
	}
	return out
}

// DisplayScore returns the single value the hit is actually ranked by, so a
// rendered "score=" is monotonic with the listed order. Precedence mirrors the
// search pipeline: the local-rerank sort key (SortScore) wins; else the
// cross-encoder score (RerankScore); else the RRF fusion score; else the raw
// cosine Score for a semantic-only search. The per-lane breakdown (sem/bm25/
// rrf/rerank) stays available behind --explain.
func (h Hit) DisplayScore() float32 {
	switch {
	case h.SortScore > 0:
		return h.SortScore
	case h.RerankScore > 0:
		return h.RerankScore
	case h.RRFScore > 0:
		return h.RRFScore
	default:
		return h.Score
	}
}

// FormatHits renders a slice of hits as a fenced CONTEXT block for
// injection into a chat completion message. Each chunk gets a header
// with path:line coordinates so the model can cite real locations.
func FormatHits(hits []Hit) string {
	var b strings.Builder
	b.WriteString("CONTEXT — relevant chunks from the project's dex index:\n\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "--- chunk %d: %s:%d-%d (%s, score=%.4f) ---\n",
			i+1, h.Path, h.StartLine, h.EndLine, h.Kind, h.Score)
		b.WriteString(h.Content)
		if !strings.HasSuffix(h.Content, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// scored holds one chunk's score during ranking. Used internally by
// both the semantic and BM25 legs; the RRF fuser then walks both lists.
type scored struct {
	id    int64
	score float32 // cosine for semantic; -bm25() for BM25 (larger = better)
}

// rrfK is the RRF dampening constant. 60 is the canonical default from
// Cormack et al. (2009); behavior is robust to values in [10, 100]. Sourced
// from the embedded calibration artifact (see calibration.yml / #467).
var rrfK = CalibratedDefaults().RRFK

// queryType classifies an incoming query to drive adaptive RRF weights.
// SACL (EMNLP 2025): query structure is a strong signal for which retrieval
// modality (lexical vs dense) dominates. Weights below are empirically tuned:
//
//	Symbol:       BM25 1.4 × dense 0.6  — exact token match dominates
//	Architecture: BM25 0.6 × dense 1.4  — semantic similarity dominates
//	NL (default): BM25 1.0 × dense 1.0  — equal contribution
type queryType int

const (
	queryNL           queryType = iota
	querySymbol                 // CamelCase / snake_case / qualified names
	queryArchitecture           // "how does", "architecture", "data flow", …
)

// QueryTypeSymbol / QueryTypeNL / QueryTypeArchitecture are the exported
// constants returned by ClassifyQueryType.
const (
	QueryTypeNL           = "nl"
	QueryTypeSymbol       = "symbol"
	QueryTypeArchitecture = "architecture"
)

// ClassifyQueryType is the exported variant of classifyQueryType.
func ClassifyQueryType(q string) string {
	switch classifyQueryType(q) {
	case querySymbol:
		return QueryTypeSymbol
	case queryArchitecture:
		return QueryTypeArchitecture
	default:
		return QueryTypeNL
	}
}

// classifyQueryType returns the queryType for q using lightweight heuristics.
// NL is the safe default when neither Symbol nor Architecture patterns fire.
func classifyQueryType(q string) queryType {
	q = strings.TrimSpace(q)
	if q == "" {
		return queryNL
	}
	lower := strings.ToLower(q)

	// Architecture: multi-token phrases about structure/design.
	// "pipeline" and "layer" were removed: they fire on commit-message jargon
	// ("CI pipeline", "query layer") and cause false positives in the eval.
	//
	// "where is" is handled specially: "where is IndexableExt defined" is a
	// symbol lookup, not an architecture query. looksLikeDefinitionQuery carves
	// out the single-identifier case before falling through to architecture.
	if strings.Contains(lower, "where is") || strings.Contains(lower, "where are") {
		fields := strings.Fields(q)
		if looksLikeDefinitionQuery(fields) {
			return querySymbol
		}
		return queryArchitecture
	}
	archPhrases := []string{
		"how does", "how is",
		"architecture", "design pattern", "data flow", "control flow",
		"module structure", "component",
	}
	for _, p := range archPhrases {
		if strings.Contains(lower, p) {
			return queryArchitecture
		}
	}

	// Symbol: single token that looks like a code identifier.
	// Fire only when the entire query is one token (no whitespace except
	// qualifiers like "Foo::Bar" or "obj.method").
	fields := strings.Fields(q)
	if len(fields) == 1 {
		tok := fields[0]
		if looksLikeIdentifier(tok) {
			return querySymbol
		}
	}
	// Two-token queries where both tokens are identifiers (e.g. "Store Search").
	if len(fields) == 2 && looksLikeIdentifier(fields[0]) && looksLikeIdentifier(fields[1]) {
		return querySymbol
	}

	// Natural-language definition lookup: "find X", "locate X", etc.
	// (The "where is X" variant is already handled above.)
	if looksLikeDefinitionQuery(fields) {
		return querySymbol
	}

	return queryNL
}

// lookupStopWords are function/location words used in natural-language symbol
// lookups ("where is X defined"). They carry no semantic signal beyond intent.
var lookupStopWords = map[string]bool{
	"where": true, "is": true, "defined": true, "declared": true,
	"located": true, "implemented": true, "find": true, "locate": true,
	"what": true, "the": true, "are": true, "function": true,
	"method": true, "struct": true, "type": true, "class": true,
	"interface": true, "in": true, "at": true,
}

// looksLikeDefinitionQuery returns true when the query has exactly one
// identifier token and all other tokens are lookup stop-words (so the intent
// is clearly "find the definition of X").
func looksLikeDefinitionQuery(fields []string) bool {
	identCount := 0
	for _, f := range fields {
		lower := strings.ToLower(f)
		if looksLikeIdentifier(f) {
			identCount++
		} else if !lookupStopWords[lower] {
			return false // unexpected non-stop word — not a definition query
		}
	}
	return identCount == 1
}

// looksLikeIdentifier returns true for tokens that match common code
// identifier patterns: CamelCase, PascalCase, snake_case, SCREAMING_CASE,
// qualified names (Foo::bar, obj.method, (*T).Method), private _foo.
func looksLikeIdentifier(tok string) bool {
	if len(tok) == 0 {
		return false
	}
	// Strip leading sigils (* & ( )).
	stripped := strings.TrimLeft(tok, "(*&")
	stripped = strings.TrimRight(stripped, ")")
	if stripped == "" {
		return false
	}
	// Must contain only identifier runes plus qualifiers . :: _ -
	for _, r := range stripped {
		if !isIdentRune(r) {
			return false
		}
	}
	// Contains at least one uppercase letter, underscore, or qualifier
	// (avoids matching plain lowercase words like "go" or "run").
	hasUpper := strings.IndexFunc(stripped, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
	hasQual := strings.ContainsAny(stripped, "._:")
	hasUnderscore := strings.Contains(stripped, "_") && len(stripped) > 3
	return hasUpper || hasQual || hasUnderscore
}

func isIdentRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-' ||
		r == '(' || r == ')' || r == '*' || r == '&'
}

// fuseLinear combines dense and BM25 scores via a min-max normalised convex
// combination:  alpha*dense_norm + (1-alpha)*bm25_norm.
// Both maps use the convention "higher = better" (BM25 scores are already
// negated before reaching here via scoreBM25).
// Items absent from a lane receive 0 after normalisation (bottom of that
// lane's range), which is conservative and avoids rewarding absent signals.
func fuseLinear(semCosine, bm25Score map[int64]float32, alpha float32) map[int64]float32 {
	if alpha <= 0 {
		alpha = 0.7
	}
	dMin, dMax := mapMinMax(semCosine)
	bMin, bMax := mapMinMax(bm25Score)

	out := make(map[int64]float32, len(semCosine)+len(bm25Score))
	for id, v := range semCosine {
		out[id] += alpha * minMaxNorm(v, dMin, dMax)
	}
	for id, v := range bm25Score {
		out[id] += (1 - alpha) * minMaxNorm(v, bMin, bMax)
	}
	return out
}

func mapMinMax(m map[int64]float32) (lo, hi float32) {
	first := true
	for _, v := range m {
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	return
}

func minMaxNorm(v, lo, hi float32) float32 {
	if hi == lo {
		return 0
	}
	return (v - lo) / (hi - lo)
}

// rrfWeights returns the BM25 and dense RRF multiplicative weights for qt.
func rrfWeights(qt queryType) (bm25W, denseW float32) {
	switch qt {
	case querySymbol:
		return 1.4, 0.6
	case queryArchitecture:
		return 0.6, 1.4
	default:
		return 1.0, 1.0
	}
}

// Search returns the top-k chunks ranked by hybrid scoring with optional
// per-file diversity via Options.MaxHitsPerFile. The canonical local quality
// rerank (noise/definition/coherence/MMR) is applied exactly once, here.
//
// Graph expansion runs BEFORE the cross-encoder rerank so graph-boosted
// semantic hits (files in the semantic pool that are also graph-adjacent) get
// the benefit of the extra RRF score. However, pure graph-only hits — files
// that are NOT in the original semantic pool — are excluded from the reranker
// and appended as breadth-only tail additions. This prevents the content-aware
// cross-encoder (which gained real text for graph hits after #361) from
// promoting graph-only files above true semantic gold files. (#394)
func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	// Over-fetch to give the graph-fuse and rerank stages headroom.
	candidateK := k * 5
	if candidateK < 30 {
		candidateK = 30
	}
	hits, err := s.SearchFused(ctx, queryVec, queryText, candidateK)
	if err != nil || len(hits) == 0 {
		return hits, err
	}

	// Record semantic-origin paths so we can separate them from pure graph
	// additions after the expansion step.
	semanticPaths := make(map[string]struct{}, len(hits))
	for i := range hits {
		semanticPaths[hits[i].Path] = struct{}{}
	}

	// Graph expansion: boosts semantic hits that are also graph-adjacent and
	// adds graph-only neighbors for breadth.
	hits = s.FuseSpreadingActivation(ctx, hits, queryVec, candidateK)

	// Split merged pool: semantic-origin hits go to the cross-encoder; pure
	// graph-only hits are held aside to avoid cross-encoder crowding. (#394)
	rerankPool := hits[:0:0]
	var graphTail []Hit
	for _, h := range hits {
		if _, ok := semanticPaths[h.Path]; ok {
			rerankPool = append(rerankPool, h)
		} else {
			graphTail = append(graphTail, h)
		}
	}

	// Final rerank-and-trim. When a ranking hook is wired (internal/retrieve's
	// cross-encoder, with its own local-rerank fallback) the store delegates to
	// it; otherwise the store applies its own local quality rerank. Either path
	// runs exactly once over the full semantic pool and trims to k. (#473)
	if s.opts.Rerank != nil {
		rerankPool, err = s.opts.Rerank(ctx, queryText, rerankPool, k)
		if err != nil {
			return nil, err
		}
	} else {
		rerankPool = ApplyLocalRerank(rerankPool, classifyQueryType(queryText) == querySymbol, s.opts.DefinitionBoost)
		if len(rerankPool) > k {
			rerankPool = rerankPool[:k]
		}
	}

	hits = append(rerankPool, graphTail...)
	if s.opts.MaxHitsPerFile > 0 {
		hits = diversify(hits, s.opts.MaxHitsPerFile)
	}
	// Enforce the top-k contract: callers size buffers and display caps
	// around k, so returning more than k hits silently violates the
	// documented bound. graphTail is appended for recall but still capped.
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// SearchFused returns RRF-fused (+ session-proximity) candidates WITHOUT the
// local quality rerank or the cross-encoder pass. Callers that fuse additional
// retrieval legs (exact-symbol lookup, graph neighbors) use this and then call
// ApplyLocalRerank once over the combined set, so the quality rerank runs
// exactly once — never twice, as happened when these callers stacked their own
// rerank on top of Store.Search's.
func (s *Store) SearchFused(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	return s.searchRaw(ctx, queryVec, queryText, k)
}

// diversify caps the number of hits per unique file path, preserving
// the existing score-based ordering. Hits beyond the cap are dropped.
func diversify(hits []Hit, maxPerFile int) []Hit {
	counts := make(map[string]int, len(hits)/2)
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if counts[h.Path] >= maxPerFile {
			continue
		}
		counts[h.Path]++
		out = append(out, h)
	}
	return out
}

// searchRaw is the internal search implementation. See Search for the
// public API. When `queryText` is non-empty AND BM25 isn't disabled,
// results from the cosine path and the FTS5/BM25 path are fused via
// Reciprocal Rank Fusion: rrf_score(id) = Σ 1/(60+rank_in_list). RRF
// is scale-free, so the two heterogenous scoring schemes compose without
// per-corpus tuning. When `queryText` is empty (or BM25 disabled),
// search degrades to semantic-only.
//
// searchRaw never reranks: it returns the fused candidate pool untouched so a
// caller can fuse extra legs and rerank the union exactly once. Ranking is
// retrieval policy and lives above the store (Search applies it; the mcp
// search tools apply it over their own union — #473).
func (s *Store) searchRaw(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 8
	}
	useBM25 := !s.opts.DisableBM25 && strings.TrimSpace(queryText) != ""

	if !useBM25 {
		// Semantic-only path. vec0 already returns rows sorted by
		// similarity desc, so no client-side sort needed.
		semScores, err := s.scoreSemantic(ctx, queryVec, k)
		if err != nil {
			return nil, err
		}
		if len(semScores) == 0 {
			return nil, nil
		}
		return s.fetchHits(ctx, semScores, scoreContext{})
	}

	// Pull more candidates per leg than the final k so fusion has
	// headroom to surface lexical-only or semantic-only hits.
	pool := k * 5
	if pool < 30 {
		pool = 30
	}
	// When the rerank hook is wired with a pool budget, cap the candidate
	// pool so we don't fetch more docs than the reranker will score.
	if s.opts.Rerank != nil && s.opts.MaxCandidatePool > 0 && pool > s.opts.MaxCandidatePool {
		pool = s.opts.MaxCandidatePool
	}

	// Semantic top-pool — sqlite-vec KNN, already sorted desc by similarity.
	semSorted, err := s.scoreSemantic(ctx, queryVec, pool)
	if err != nil {
		return nil, err
	}
	semCosine := make(map[int64]float32, len(semSorted))
	semRank := make(map[int64]int, len(semSorted))
	for i, sc := range semSorted {
		semCosine[sc.id] = sc.score
		semRank[sc.id] = i + 1
	}

	// BM25 top-pool.
	bm25Scores, err := s.scoreBM25(ctx, queryText, pool)
	if err != nil {
		// If FTS5 chokes on the query (e.g. unbalanced quotes), fall
		// back to semantic-only rather than failing the user's search.
		bm25Scores = nil
	}
	bm25Rank := make(map[int64]int, len(bm25Scores))
	bm25Score := make(map[int64]float32, len(bm25Scores))
	for i, sc := range bm25Scores {
		bm25Rank[sc.id] = i + 1
		bm25Score[sc.id] = sc.score
	}

	// Fill cosine for BM25-only fused IDs so Hit.Score stays populated
	// for every result, not just semantic-leg ones. The set is bounded
	// by `pool` and usually small in practice (high lexical/semantic
	// overlap), so the extra round-trip is cheap.
	var missing []int64
	for id := range bm25Rank {
		if _, ok := semCosine[id]; !ok {
			missing = append(missing, id)
		}
	}
	if filled, err := s.scoreSemanticForIDs(ctx, queryVec, missing); err == nil {
		for id, sim := range filled {
			semCosine[id] = sim
		}
	}

	// Fuse dense and BM25 lanes.
	var rrf map[int64]float32
	if s.opts.FusionMode == FusionLinear {
		// Convex combination on min-max normalised scores.
		// alpha (DEX_FUSION_ALPHA) is the dense weight; 0 defaults to 0.7 (#317).
		rrf = fuseLinear(semCosine, bm25Score, s.opts.FusionAlpha)
	} else {
		// Weighted RRF. Weights are query-type-adaptive (SACL EMNLP 2025):
		// symbol queries favour lexical, architecture queries favour dense,
		// NL queries use equal weights. Scale-free property is preserved —
		// the constant multipliers cancel in relative ranking.
		bm25W, denseW := rrfWeights(classifyQueryType(queryText))
		rrf = make(map[int64]float32, len(semRank)+len(bm25Rank))
		for id, r := range semRank {
			rrf[id] += denseW / float32(rrfK+r)
		}
		for id, r := range bm25Rank {
			rrf[id] += bm25W / float32(rrfK+r)
		}
	}

	// Batch-fetch paths for the full fused pool — used by noise penalties,
	// session proximity boost, and MMR diversity below. Fast PK lookup.
	allIDs := make([]int64, 0, len(rrf))
	for id := range rrf {
		allIDs = append(allIDs, id)
	}
	pathFor, _ := s.fetchPathsForIDs(ctx, allIDs) // degrade gracefully on error

	// Note: noise penalties / definition / coherence / MMR are NOT applied
	// here — they belong to the single canonical reranker (ApplyLocalRerank),
	// invoked once at the tail when applyLocal is set. Applying them inline
	// here too would double-penalize callers that rerank again downstream.

	// Session graph proximity boost (#118): files the agent recently
	// touched in this session get an extra RRF addend, making search
	// context-aware without explicit path filtering.
	s.applyProximityBonus(ctx, rrf, pathFor)

	fused := make([]scored, 0, len(rrf))
	for id, r := range rrf {
		fused = append(fused, scored{id, r})
	}
	sort.Slice(fused, func(i, j int) bool { return fused[i].score > fused[j].score })

	// Lane provenance (#707): an id present in semRank surfaced through the
	// dense leg, one in bm25Rank through the lexical leg; a hit in both is a
	// two-lane agreement. Built from the rank maps (not semCosine, which is
	// back-filled for BM25-only ids and would over-claim the vector lane).
	lanes := make(map[int64]LaneSet, len(rrf))
	for id := range semRank {
		lanes[id] = lanes[id].With(LaneVector)
	}
	for id := range bm25Rank {
		lanes[id] = lanes[id].With(LaneBM25)
	}

	// Hand back the FULL fused candidate pool (k*5, not trimmed to k) without
	// any rerank, so the caller can fuse additional legs and then rerank the
	// union exactly once. Trimming or reranking here would starve the
	// downstream reranker of the candidate pool it needs (#473).
	return s.fetchHits(ctx, fused, scoreContext{semCosine: semCosine, bm25Score: bm25Score, lanes: lanes})
}

// inPlaceholders returns a comma-separated list of n SQL "?" bind vars,
// e.g. inPlaceholders(3) == "?,?,?".
func inPlaceholders(n int) string {
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// scoreSemantic returns up to `limit` chunks ranked by cosine similarity
// to queryVec, best first. Runs as a single KNN query against the
// sqlite-vec `chunk_vecs` virtual table; vec0 returns rows sorted by
// distance ascending, which is similarity descending — no client-side
// sort needed.
func (s *Store) scoreSemantic(ctx context.Context, queryVec []float32, limit int) ([]scored, error) {
	// An empty query vector means "no semantic leg": degraded search (when the
	// embedding service is offline) passes a nil vector + query text to run
	// BM25-only through the fusion path. Distinct from a zero vector below,
	// which is a real embedding gone wrong and stays an error.
	if len(queryVec) == 0 {
		return nil, nil
	}
	if d := s.dim.Load(); d != 0 && int64(len(queryVec)) != d {
		return nil, fmt.Errorf("query dim %d != index dim %d", len(queryVec), d)
	}
	// Reject all-zero queries up front. vec0's cosine path would otherwise
	// produce NaN distances on a zero vector and surface nonsense rankings.
	// Done before the empty-index early-return so callers get a clear error
	// even when there's nothing to search yet.
	allZero := true
	for _, x := range queryVec {
		if x != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, fmt.Errorf("query vector is zero")
	}
	if limit <= 0 || s.dim.Load() == 0 {
		return nil, nil
	}
	qBlob := encodeVec(queryVec)
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, distance FROM chunk_vecs
		 WHERE embedding MATCH %s AND k = ?
		 ORDER BY distance`, s.vecMatchExpr()),
		qBlob, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]scored, 0, limit)
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		// Cosine distance ∈ [0, 2]; convert to similarity ∈ [-1, 1] so
		// callers can keep the "larger = better" convention shared with
		// the BM25 leg (which flips bm25() sign for the same reason).
		out = append(out, scored{id, float32(1 - dist)})
	}
	return out, rows.Err()
}

// scoreSemanticForIDs fills in cosine similarity for a specific set of
// chunk IDs that the vec0 top-K query missed (BM25-only fused hits).
// Uses sqlite-vec's scalar vec_distance_cosine() so we can keep Hit.Score
// populated even for hits that surfaced purely through the lexical leg.
// Returns a partial map; callers must tolerate missing entries.
func (s *Store) scoreSemanticForIDs(ctx context.Context, queryVec []float32, ids []int64) (map[int64]float32, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, encodeVec(queryVec))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, vec_distance_cosine(?, vec) FROM chunks WHERE id IN (`+inPlaceholders(len(ids))+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]float32, len(ids))
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		out[id] = float32(1 - dist)
	}
	return out, rows.Err()
}

// scoreBM25 runs the FTS5 / BM25 leg of hybrid search. Returns the
// top-`limit` chunk IDs ordered by BM25 rank (best first), with the
// score field set to -bm25() (so larger = better, consistent with the
// cosine path's convention).
//
// Kind weighting: bm25() returns negative numbers (more negative =
// better). Multiplying by 0.7 for `window` chunks (free-form line
// slices, dominated by Markdown/README content) pushes them toward
// zero — i.e. worse rank — so a README that happens to list every
// identifier the codebase exposes can't crowd out the actual
// definition site. Structural chunks (function_declaration etc.) and
// `orphan` chunks (top-level const/var/import we'd lose otherwise)
// keep their full BM25 weight.
func (s *Store) scoreBM25(ctx context.Context, queryText string, limit int) ([]scored, error) {
	matchExpr := buildFTSQuery(queryText, s.opts.FTSMode)
	if matchExpr == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT chunks_fts.rowid,
		        bm25(chunks_fts, 1.0, 2.0, 0.5) * CASE chunks.kind
		            WHEN 'window' THEN 0.7
		            ELSE 1.0
		          END AS weighted_rank
		   FROM chunks_fts
		   JOIN chunks ON chunks.id = chunks_fts.rowid
		   WHERE chunks_fts MATCH ?
		   ORDER BY weighted_rank
		   LIMIT ?`,
		matchExpr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]scored, 0, limit)
	for rows.Next() {
		var id int64
		var bm float64
		if err := rows.Scan(&id, &bm); err != nil {
			return nil, err
		}
		// bm25() returns negative rank by convention (smaller = better).
		// Flip the sign so larger = better, matching cosine.
		out = append(out, scored{id, float32(-bm)})
	}
	return out, rows.Err()
}

// buildFTSQuery turns a natural-language query into an FTS5 MATCH
// expression.
//
// Tokenization mirrors the schema's `unicode61` tokenizer: anything
// that's a Unicode letter, digit, or `_` is part of an identifier.
// This keeps non-ASCII names (`ParseRFC3339Núñez`, `ユーザー認証`) from
// being silently dropped — the ASCII-only filter that used to live
// here lost those tokens entirely, so BM25 contributed nothing on
// non-ASCII queries.
//
// Quoted substrings survive as FTS5 phrases: `"package boundary"` in
// the user query becomes `"package boundary"` in the MATCH expression
// (multi-token, ordered). Useful for forcing precision on a known
// phrase even when the overall mode is OR.
//
// Join operator follows mode:
//   - Auto: AND for 1–2 terms (symbol-shaped lookup), OR for 3+
//     (natural-language question where AND would too often return zero
//     hits).
//   - AND / OR: explicit override.
func buildFTSQuery(q string, mode FTSMode) string {
	isIdentRune := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}
	// tokenize splits a span on non-identifier runes, dropping single-rune
	// tokens (they're noisy in BM25 and FTS5 phrases must be non-empty).
	tokenize := func(span string) []string {
		var toks []string
		var b strings.Builder
		flush := func() {
			t := b.String()
			b.Reset()
			runes := 0
			for range t {
				runes++
				if runes >= 2 {
					break
				}
			}
			if runes >= 2 {
				toks = append(toks, t)
			}
		}
		for _, r := range span {
			if isIdentRune(r) {
				b.WriteRune(r)
			} else {
				flush()
			}
		}
		flush()
		return toks
	}

	var terms []string // each is a complete FTS5 term: `"word"` or `"w1 w2"`
	runes := []rune(q)
	i := 0
	for i < len(runes) {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		if runes[i] == '"' {
			// Find closing quote; tokenize contents and emit one phrase.
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				j++
			}
			phraseToks := tokenize(string(runes[i+1 : j]))
			if len(phraseToks) > 0 {
				terms = append(terms, `"`+strings.Join(phraseToks, " ")+`"`)
			}
			i = j
			if i < len(runes) {
				i++ // step past the closing quote (or end of input)
			}
			continue
		}
		// Read until next whitespace or quote.
		start := i
		for i < len(runes) && !unicode.IsSpace(runes[i]) && runes[i] != '"' {
			i++
		}
		for _, t := range tokenize(string(runes[start:i])) {
			terms = append(terms, expandSPLADE(strings.ToLower(t), expandCamelTerm(`"`+t+`"`)))
		}
	}

	if len(terms) == 0 {
		return ""
	}
	joiner := " OR "
	switch mode {
	case FTSModeAND:
		joiner = " AND "
	case FTSModeOR:
		joiner = " OR "
	default: // Auto
		if len(terms) < 3 {
			joiner = " AND "
		}
	}
	return strings.Join(terms, joiner)
}

// scoreContext carries the per-id score maps produced by the hybrid /
// reranked search pipeline. All fields are optional (nil = not available).
type scoreContext struct {
	semCosine   map[int64]float32 // raw cosine scores from the semantic leg
	bm25Score   map[int64]float32 // BM25 scores from the FTS leg
	rrfScore    map[int64]float32 // RRF fusion scores (non-nil only on reranked path)
	rerankScore map[int64]float32 // cross-encoder scores (non-nil only on reranked path)
	lanes       map[int64]LaneSet // per-id lane provenance (vector/bm25); nil = stamp vector (#707)
}

// fetchHits issues one SELECT to get content for the ranked IDs, then
// assembles Hit values with scores from sc.
//   - sc.semCosine / sc.bm25Score: nil in semantic-only mode.
//   - sc.rrfScore: non-nil on the reranked path; ranked[i].score is the
//     rerank score in that case, so RRFScore must come from the map.
//   - sc.rerankScore: non-nil on the reranked path; populates Hit.RerankScore.
func (s *Store) fetchHits(ctx context.Context, ranked []scored, sc scoreContext) ([]Hit, error) {
	if len(ranked) == 0 {
		return nil, nil
	}
	idArgs := make([]any, len(ranked))
	for i, r := range ranked {
		idArgs[i] = r.id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, kind, name, start_line, end_line, content FROM chunks WHERE id IN (`+inPlaceholders(len(idArgs))+`)`,
		idArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[int64]Hit, len(ranked))
	for rows.Next() {
		var id int64
		var h Hit
		if err := rows.Scan(&id, &h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content); err != nil {
			return nil, err
		}
		byID[id] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(ranked))
	for _, r := range ranked {
		h, ok := byID[r.id]
		if !ok {
			continue
		}
		if sc.semCosine != nil {
			h.Score = sc.semCosine[r.id]
			if sc.rrfScore != nil {
				h.RRFScore = sc.rrfScore[r.id]
			} else {
				h.RRFScore = r.score
			}
		} else {
			h.Score = r.score
		}
		if sc.bm25Score != nil {
			h.BM25Score = sc.bm25Score[r.id]
		}
		if sc.rerankScore != nil {
			h.RerankScore = sc.rerankScore[r.id]
		}
		// Lane provenance (#707). A populated map carries the exact lanes that
		// surfaced each id (vector and/or bm25); a nil map means the caller ran
		// a single-lane path where every hit came from the dense leg.
		if sc.lanes != nil {
			h.Lanes = sc.lanes[r.id]
		} else {
			h.Lanes = h.Lanes.With(LaneVector)
		}
		out = append(out, h)
	}
	return out, nil
}

// FindSymbol returns chunks whose `name` column exactly matches the
// given identifier. Results are ordered by (path, start_line). Uses a
// SQL index scan — no embedding required — so it is fast regardless of
// index size.
//
// When the chunks table yields zero hits, falls back to a graph_nodes
// scan. The Go-graph layer indexes types and struct fields that don't
// produce standalone chunks (the chunker emits chunks per function/
// method/class, not per field), so a query like `MaxFileSize` finds
// the field via the graph even though chunks has nothing. Graph-fallback
// hits carry path + line range but empty Content, since graph nodes
// only point at offsets — agents can Read the range for the body.
func (s *Store) FindSymbol(ctx context.Context, name string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 10
	}
	// LEFT JOIN graph_nodes on chunk_id surfaces centrality columns for
	// the (typically single) graph node bound to each chunk. When the
	// graph hasn't been built — or the chunk isn't a function/method —
	// the COALESCEd zeros sink the row to the natural path-order tail,
	// preserving the pre-centrality default.
	//
	// Sort key: pagerank DESC, in_degree DESC, then path/line for
	// determinism on ties. Centrality is per-symbol, so two callers
	// asking "search_symbol Indexer" land on the SAME top result every
	// run, instead of whichever chunk happens to come first in path
	// order.
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.path, c.kind, c.name, c.start_line, c.end_line, c.content,
		        COALESCE(g.in_degree, 0), COALESCE(g.out_degree, 0),
		        COALESCE(g.cross_pkg_callers, 0), COALESCE(g.pagerank, 0),
		        COALESCE(g.betweenness, 0), COALESCE(g.signature, '')
		 FROM chunks c
		 LEFT JOIN graph_nodes g ON g.chunk_id = c.id
		 WHERE c.name = ?
		 ORDER BY COALESCE(g.pagerank, 0) DESC,
		          COALESCE(g.in_degree, 0) DESC,
		          c.path, c.start_line
		 LIMIT ?`,
		name, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var id int64
		var h Hit
		if err := rows.Scan(&id, &h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content,
			&h.InDegree, &h.OutDegree, &h.CrossPkgCallers, &h.PageRank, &h.Betweenness, &h.Signature); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	return s.findSymbolInGraph(ctx, name, k)
}

// findSymbolInGraph queries the Go-graph layer for nodes whose `name`
// column matches exactly. Used as a fallback by FindSymbol when the
// chunks table has nothing — covers types, struct fields, and other
// entities that don't produce standalone chunks. Returns nil on
// missing graph table (older index versions) rather than failing the
// surrounding lookup.
func (s *Store) findSymbolInGraph(ctx context.Context, name string, k int) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, name, file_path, start_line, end_line,
		        in_degree, out_degree, cross_pkg_callers, pagerank,
		        COALESCE(betweenness, 0), COALESCE(signature, '')
		 FROM graph_nodes
		 WHERE name = ? AND file_path != '' AND start_line > 0
		 ORDER BY pagerank DESC, in_degree DESC, file_path, start_line LIMIT ?`,
		name, k)
	if err != nil {
		// graph_nodes may not exist on older indexes — degrade silently.
		return nil, nil //nolint:nilerr
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.Name, &h.Path, &h.StartLine, &h.EndLine,
			&h.InDegree, &h.OutDegree, &h.CrossPkgCallers, &h.PageRank, &h.Betweenness, &h.Signature); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FindSymbolCandidates returns up to k distinct chunk names whose
// `name` column contains `query` as a substring. Ordered by length
// (shorter ≈ closer-in-spirit) then alphabetically. Intended as a
// "did you mean" surface for search_symbol misses — callers should pass
// the exact-name lookup query and surface the results in a hint so
// the agent can retry with a real identifier instead of guessing.
func (s *Store) FindSymbolCandidates(ctx context.Context, query string, k int) ([]string, error) {
	if k <= 0 {
		k = 5
	}
	if query == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT name FROM chunks
		 WHERE name LIKE '%' || ? || '%' AND name != '' AND name != ?
		 ORDER BY length(name), name LIMIT ?`,
		query, query, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// RelatedChunks returns the top-k chunks most similar to the chunk at
// (path, startLine), excluding the source chunk itself. Issues one vec0
// KNN query with k+1 candidates so we can drop the source (which always
// ranks first at distance 0). Returns an error if no chunk is found at
// the given location.
func (s *Store) RelatedChunks(ctx context.Context, path string, startLine int, k int) ([]Hit, error) {
	if k <= 0 {
		k = 8
	}
	var blob []byte
	var sourceID int64
	// Find the most specific chunk whose span contains startLine.
	// Exact-start match is preferred; when backfillComments shifted the
	// stored start_line to a leading doc comment, callers passing the
	// declaration line still resolve to the right chunk.
	err := s.db.QueryRowContext(ctx,
		`SELECT id, vec FROM chunks
		 WHERE path = ? AND start_line <= ? AND end_line >= ?
		 ORDER BY (end_line - start_line) ASC LIMIT 1`,
		path, startLine, startLine).Scan(&sourceID, &blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("no chunk at %s:%d", path, startLine)
		}
		return nil, err
	}
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("vec blob length %d not divisible by 4", len(blob))
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, distance FROM chunk_vecs
		 WHERE embedding MATCH %s AND k = ?
		 ORDER BY distance`, s.vecMatchExpr()),
		blob, k+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scores := make([]scored, 0, k)
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		if id == sourceID {
			continue
		}
		scores = append(scores, scored{id, float32(1 - dist)})
		if len(scores) >= k {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.fetchHits(ctx, scores, scoreContext{})
}

// ChunkAt returns the most specific indexed chunk whose span contains
// startLine in path. Used by search_similar to obtain the source chunk's
// content for embedding. Returns an error matching "no chunk at" when the
// location isn't indexed.
func (s *Store) ChunkAt(ctx context.Context, path string, startLine int) (Hit, error) {
	var h Hit
	err := s.db.QueryRowContext(ctx,
		`SELECT path, kind, name, start_line, end_line, content FROM chunks
		 WHERE path = ? AND start_line <= ? AND end_line >= ?
		 ORDER BY (end_line - start_line) ASC LIMIT 1`,
		path, startLine, startLine).
		Scan(&h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Hit{}, fmt.Errorf("no chunk at %s:%d", path, startLine)
		}
		return Hit{}, err
	}
	return h, nil
}

// PathIndexed reports whether the given path has any indexed chunks.
func (s *Store) PathIndexed(ctx context.Context, path string) (bool, error) {
	var cnt int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE path = ? LIMIT 1`, path).Scan(&cnt)
	if err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// FindSymbolInPath returns chunks whose name matches sym within the given file.
// symbolsMatch logic (receiver stripping) is handled by the caller; here we do
// exact and suffix matches to cover "Check" matching "(*Server).Check".
func (s *Store) FindSymbolInPath(ctx context.Context, path, sym string) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, kind, name, start_line, end_line, content FROM chunks
		 WHERE path = ? AND (name = ? OR name LIKE ?)
		 ORDER BY start_line ASC`,
		path, sym, "%."+sym)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// CodeFilePaths returns every real code file in the chunks table
// mapped to its inferred line count (max end_line across all its chunks).
// Used by overview to enumerate the indexed codebase without loading content.
func (s *Store) CodeFilePaths(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, MAX(end_line)
		FROM chunks
		WHERE path != ''
		GROUP BY path
		ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var p string
		var lc int
		if err := rows.Scan(&p, &lc); err != nil {
			return nil, err
		}
		out[p] = lc
	}
	return out, rows.Err()
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}
