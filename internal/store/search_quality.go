package store

import (
	"context"
	"math"
	"strings"
)

// noisePenalty returns a score multiplier for a file path.
// Test files, legacy code, examples, and barrel/stub files are
// down-ranked so agents hit implementation files first.
// Multipliers from CoRNStack ICLR 2025 / lean-ctx search_reranking.
func noisePenalty(path string) float64 {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "_test") ||
		strings.Contains(p, "/tests/") ||
		strings.HasSuffix(p, "_test.go"):
		return 0.3
	case strings.Contains(p, "legacy") ||
		strings.Contains(p, "deprecated") ||
		strings.Contains(p, "compat"):
		return 0.3
	case strings.Contains(p, "/examples/") || strings.HasPrefix(p, "examples/") ||
		strings.Contains(p, "/fixtures/") || strings.HasPrefix(p, "fixtures/") ||
		strings.Contains(p, "/testdata/") || strings.HasPrefix(p, "testdata/"):
		return 0.3
	case strings.HasSuffix(p, ".d.ts") ||
		strings.HasSuffix(p, ".pyi"):
		return 0.7
	}
	return 1.0
}

// applyMMR applies Maximal Marginal Relevance (Carbonell & Goldstein, SIGIR 1998)
// to the sorted fused pool. Each additional chunk from the same file gets its
// effective score multiplied by mmrDecay^n where n is the number of chunks from
// that file already selected. This prevents one large file from dominating the
// top-k results. Returns at most k items.
func applyMMR(pool []scored, pathFor map[int64]string, k int) []scored {
	const mmrDecay = 0.5

	if len(pool) <= k {
		return pool
	}

	fileCount := make(map[string]int, k)
	out := make([]scored, 0, k)
	remaining := make([]scored, len(pool))
	copy(remaining, pool)

	for len(out) < k && len(remaining) > 0 {
		best := 0
		bestScore := float32(-1)
		for i, c := range remaining {
			n := fileCount[pathFor[c.id]]
			var s float32
			if n == 0 {
				s = c.score
			} else {
				s = c.score * float32(math.Pow(mmrDecay, float64(n)))
			}
			if s > bestScore {
				bestScore = s
				best = i
			}
		}
		out = append(out, remaining[best])
		fileCount[pathFor[remaining[best].id]]++
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return out
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
