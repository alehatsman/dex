package store

import (
	"context"
	"sort"
	"strings"
)

// ApplyLocalRerank is the single canonical post-RRF quality reranker. It is the
// one place noise penalties, definition/coherence boosts, and MMR diversity are
// applied — every search surface (Store.Search, and the MCP fusing tools that
// call SearchFused then fuse extra legs) funnels through it exactly once, so the
// ordering is identical regardless of entry point.
//
// Passes, operating on RRFScore (falling back to cosine Score when RRF didn't
// run, i.e. semantic-only search):
//  1. per-hit: path noise penalty (multiplicative) + definition boost
//     (1.5× for symbol queries on declaration-kind chunks)
//  2. file coherence: chunks from files with ≥2 hits get a 1.15× boost
//  3. MMR diversity: chunks beyond the 2nd from the same file decay 0.7× per
//     excess, preventing one large file from dominating the top-k
//
// When a cross-encoder already ordered the pool (any hit carries a non-zero
// RerankScore), the cross-encoder result is authoritative and this is a no-op —
// it must not clobber the reranker's ordering by re-sorting on RRFScore.
//
// Multipliers from CoRNStack ICLR 2025 / lean-ctx search_reranking.
func ApplyLocalRerank(hits []Hit, isSymbolQuery bool) []Hit {
	if len(hits) == 0 {
		return hits
	}
	// Respect a cross-encoder ordering if one ran; don't re-sort over it.
	for i := range hits {
		if hits[i].RerankScore != 0 {
			return hits
		}
	}

	// Rerank on a scratch score per hit — seeded from RRFScore, falling back to
	// cosine Score when RRF didn't run (semantic-only search). The Hit.RRFScore
	// field is left untouched so the "RRF didn't run → RRFScore == 0" contract
	// holds for callers that inspect it (e.g. DisableBM25).
	type ranked struct {
		h Hit
		s float32
	}
	arr := make([]ranked, len(hits))
	for i := range hits {
		base := hits[i].RRFScore
		if base == 0 {
			base = hits[i].Score
		}
		arr[i] = ranked{hits[i], base}
	}

	// Pass 1: per-hit signals.
	for i := range arr {
		if pen := pathPenalty(arr[i].h.Path); pen != 1.0 {
			arr[i].s *= float32(pen)
		}
		if isSymbolQuery && isDefinitionKind(arr[i].h.Kind) {
			arr[i].s *= 1.5
		}
	}

	// Pass 2: file coherence — boost all chunks from files with ≥2 hits.
	fileCnt := make(map[string]int, len(arr))
	for i := range arr {
		fileCnt[arr[i].h.Path]++
	}
	for i := range arr {
		if fileCnt[arr[i].h.Path] >= 2 {
			arr[i].s *= 1.15
		}
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].s > arr[j].s })

	// Pass 3: MMR diversity — decay chunks beyond the 2nd from the same file.
	seen := make(map[string]int, len(arr))
	for i := range arr {
		seen[arr[i].h.Path]++
		for excess := seen[arr[i].h.Path] - 2; excess > 0; excess-- {
			arr[i].s *= 0.7
		}
	}
	sort.SliceStable(arr, func(i, j int) bool { return arr[i].s > arr[j].s })

	out := make([]Hit, len(arr))
	for i := range arr {
		out[i] = arr[i].h
	}
	return out
}

// pathPenalty returns a multiplicative down-rank factor in (0,1] for paths that
// are typically low-signal for implementation searches (CoRNStack ICLR 2025
// multipliers, extended with compat/legacy/barrel/stub tiers). Penalties stack
// multiplicatively, so a test file inside a legacy dir gets 0.3 × 0.3 = 0.09×.
//
// Self-contained (no dependency on the mcp package) so the store layer owns its
// own path classification.
func pathPenalty(path string) float64 {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	penalty := 1.0
	if isTestPath(p) {
		penalty *= 0.3
	}
	if isCompatLegacyPath(p) {
		penalty *= 0.3
	}
	if isExampleDocsPath(p) {
		penalty *= 0.3
	}
	if isReexportBarrel(p) {
		penalty *= 0.5
	}
	if isTypeStub(p) {
		penalty *= 0.7
	}
	return penalty
}

// isTestPath mirrors the mcp package's test/fixture classification: a path is
// "test" if it lives in a fixture/mock/stub directory or its basename carries a
// per-language test/spec suffix. Kept in sync with mcp.isFixturePathRaw +
// mcp.pathTags so the reranker demotes exactly what the suggested-reads ranker
// does.
func isTestPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "testdata", "__fixtures__",
			"testutil", "mock", "mocks", "stub", "stubs", "fake", "fakes":
			return true
		}
	}
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".test.jsx") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.tsx") ||
		strings.HasSuffix(base, ".spec.js") ||
		strings.HasSuffix(base, ".spec.jsx") ||
		strings.HasSuffix(base, "_test.py") ||
		(strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) ||
		strings.HasSuffix(base, "_spec.rb") ||
		strings.HasSuffix(base, "_test.rs")
}

func isCompatLegacyPath(p string) bool {
	return hasPathSegment(p, "compat") ||
		hasPathSegment(p, "legacy") ||
		hasPathSegment(p, "deprecated")
}

func isExampleDocsPath(p string) bool {
	return hasPathSegment(p, "examples") ||
		hasPathSegment(p, "example") ||
		hasPathSegment(p, "demo") ||
		hasPathSegment(p, "docs_src") ||
		hasPathSegment(p, "samples") ||
		hasPathSegment(p, "tutorials") ||
		hasPathSegment(p, "cookbook")
}

// hasPathSegment returns true when any /-delimited segment of p equals seg.
func hasPathSegment(p, seg string) bool {
	for {
		idx := strings.Index(p, seg)
		if idx < 0 {
			return false
		}
		if idx > 0 && p[idx-1] != '/' {
			p = p[idx+len(seg):]
			continue
		}
		end := idx + len(seg)
		if end == len(p) || p[end] == '/' {
			return true
		}
		p = p[end:]
	}
}

func isReexportBarrel(p string) bool {
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	return base == "index.ts" || base == "index.tsx" ||
		base == "__init__.py" ||
		base == "package-info.java" ||
		base == "mod.rs" // Rust re-export modules
}

func isTypeStub(p string) bool {
	return strings.HasSuffix(p, ".d.ts") || strings.HasSuffix(p, ".pyi")
}

// isDefinitionKind returns true for tree-sitter node types that represent a
// declaration site (function, method, struct, class, interface, type).
func isDefinitionKind(kind string) bool {
	if strings.HasSuffix(kind, ":window") {
		return false
	}
	return strings.Contains(kind, "function") ||
		strings.Contains(kind, "method") ||
		strings.Contains(kind, "class") ||
		strings.Contains(kind, "struct") ||
		strings.Contains(kind, "interface") ||
		strings.Contains(kind, "type_decl") ||
		strings.Contains(kind, "impl_item")
}

// fetchPathsForIDs returns a map from chunk ID to file path for the given
// set of IDs. Uses the primary-key index so it's a fast point lookup.
// Missing IDs are silently omitted; callers must handle absent entries.
func (s *Store) fetchPathsForIDs(ctx context.Context, ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path FROM chunks WHERE id IN (`+inPlaceholders(len(ids))+`)`, //nolint:gosec
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		out[id] = path
	}
	return out, rows.Err()
}

// sessionProximityBonus returns an extra RRF addend for chunks in session-touched
// files. Files the agent recently read get an extra 1/(rrfK+0) term — equivalent
// to being ranked first in an additional virtual leg.
// Only fires when an active session with recorded files exists.
func (s *Store) sessionProximityBonus(ctx context.Context, pathFor map[int64]string) map[int64]float32 {
	if len(pathFor) == 0 {
		return nil
	}
	sess, ok, err := s.SessionGet(ctx)
	if err != nil || !ok || len(sess.Files) == 0 {
		return nil
	}

	// Build set of recently-touched paths (most-recent 20).
	limit := len(sess.Files)
	if limit > 20 {
		limit = 20
	}
	sessionPaths := make(map[string]struct{}, limit)
	for _, f := range sess.Files[:limit] {
		sessionPaths[f.Path] = struct{}{}
	}

	out := make(map[int64]float32)
	for id, p := range pathFor {
		if _, touched := sessionPaths[p]; touched {
			// Bonus equivalent to rank-0 in a third RRF leg.
			out[id] = 1.0 / float32(rrfK)
		}
	}
	return out
}
