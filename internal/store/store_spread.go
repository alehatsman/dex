package store

import (
	"context"
	"sort"
)

// Spreader is the assemble-related-files surface of a Store (#688).
// *Store satisfies Spreader — pass one directly wherever a Spreader is needed.
type Spreader interface {
	AssembleRelated(ctx context.Context, seeds []string, n int) ([]string, error)
}

// compile-time check: *Store must implement Spreader.
var _ Spreader = (*Store)(nil)

const (
	assembleHops  = 3
	assembleDecay = 0.6
)

// AssembleRelated runs multi-hop spreading activation over the union of the
// static call/import graph and learned co-access (Hebbian) edges (#688),
// seeded on the given file paths. Each hop multiplies the activation score by
// assembleDecay (0.6); files reached earlier rank higher. Returns up to n file
// paths ordered by descending activation score, excluding seeds.
func (s *Store) AssembleRelated(ctx context.Context, seeds []string, n int) ([]string, error) {
	if len(seeds) == 0 || n <= 0 {
		return nil, nil
	}

	visited := make(map[string]struct{}, len(seeds)*4)
	scores := make(map[string]float64, len(seeds)*4)

	frontier := make([]string, 0, len(seeds))
	for _, p := range seeds {
		if p != "" {
			visited[p] = struct{}{}
			frontier = append(frontier, p)
		}
	}
	if len(frontier) == 0 {
		return nil, nil
	}

	hopScore := 1.0
	for hop := 0; hop < assembleHops && len(frontier) > 0; hop++ {
		hopScore *= assembleDecay

		graphHits, err := s.graphFileNeighbors(ctx, frontier)
		if err != nil {
			return nil, err
		}

		coHits, _ := s.CoAccessNeighbors(ctx, frontier, n*3)

		var nextFrontier []string
		for _, p := range append(graphHits, coHits...) {
			if _, seen := visited[p]; seen {
				continue
			}
			visited[p] = struct{}{}
			scores[p] = hopScore
			nextFrontier = append(nextFrontier, p)
		}
		frontier = nextFrontier
	}

	if len(scores) == 0 {
		return nil, nil
	}

	type entry struct {
		path  string
		score float64
	}
	ranked := make([]entry, 0, len(scores))
	for p, sc := range scores {
		ranked = append(ranked, entry{p, sc})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].path < ranked[j].path
	})

	if len(ranked) > n {
		ranked = ranked[:n]
	}
	out := make([]string, len(ranked))
	for i, r := range ranked {
		out[i] = r.path
	}
	return out, nil
}

// graphFileNeighbors returns all 1-hop file-level neighbors of the seed files
// via graph_edges, ordered by descending PageRank. Seed files themselves may
// appear in results; callers filter using their visited set.
func (s *Store) graphFileNeighbors(ctx context.Context, seeds []string) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	ph := inPlaceholders(len(seeds))
	args := make([]any, len(seeds)+1)
	for i, p := range seeds {
		args[i] = p
	}
	limit := len(seeds) * 20
	if limit < 100 {
		limit = 100
	}
	args[len(seeds)] = limit

	q := `WITH seed_ids AS (SELECT id FROM graph_nodes WHERE file_path IN (` + //nolint:gosec
		ph + `)), neighbor_ids AS (` +
		`SELECT dst_id AS id FROM graph_edges WHERE src_id IN (SELECT id FROM seed_ids) ` +
		`UNION SELECT src_id AS id FROM graph_edges WHERE dst_id IN (SELECT id FROM seed_ids)` +
		`) SELECT DISTINCT gn.file_path FROM graph_nodes gn ` +
		`JOIN neighbor_ids ni ON gn.id = ni.id WHERE gn.file_path != '' ` +
		`ORDER BY gn.pagerank DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}
