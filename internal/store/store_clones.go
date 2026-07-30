package store

import (
	"context"
	"fmt"
	"sort"
)

// CloneMember is one code block participating in a duplication cluster.
type CloneMember struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
}

// CloneCluster is a set of >=2 code blocks that are mutually near-duplicate in
// embedding space. Similarity is the weakest duplicate edge inside the cluster
// — a floor on how alike its members are.
type CloneCluster struct {
	Members    []CloneMember `json:"members"`
	Similarity float32       `json:"similarity"`
}

// CloneOpts bounds a clone scan.
type CloneOpts struct {
	PathPrefix    string  // restrict candidates to paths under this prefix ("" = whole repo)
	Threshold     float32 // min cosine similarity for a duplicate edge (default 0.90)
	MinLines      int     // ignore blocks shorter than this many lines (default 6)
	K             int     // neighbours probed per candidate (default 10)
	MaxCandidates int     // cap on candidate blocks scanned (default 2000)
	MaxClusters   int     // cap on clusters returned (default 20)
}

func (o *CloneOpts) applyDefaults() {
	if o.Threshold <= 0 {
		o.Threshold = 0.90
	}
	if o.MinLines <= 0 {
		o.MinLines = 6
	}
	if o.K <= 0 {
		o.K = 10
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = 2000
	}
	if o.MaxClusters <= 0 {
		o.MaxClusters = 20
	}
}

type cloneCand struct {
	path, kind, name string
	start, end       int
	vec              []byte
}

// CloneClusters scans structural code blocks (functions/methods) and returns
// clusters of semantically near-duplicate ones. All work is in-store: it reuses
// the sqlite-vec vectors already indexed for search — no embedder round-trip.
//
// Shape: pull long-enough function/method chunks as candidates, KNN each
// candidate against chunk_vecs, keep neighbour edges at/above the similarity
// threshold (skipping a block's overlap with itself), then union-find the edges
// into connected components. Each component of >=2 members is a duplication
// hotspot. Cost is O(candidates × KNN); MaxCandidates caps the worst case.
func (s *Store) CloneClusters(ctx context.Context, opts CloneOpts) ([]CloneCluster, error) {
	opts.applyDefaults()

	// 1. Candidate blocks: functions/methods long enough to be worth comparing.
	q := `SELECT id, path, kind, name, start_line, end_line, vec FROM chunks
	       WHERE length(vec) > 0
	         AND (kind LIKE '%function%' OR kind LIKE '%method%')
	         AND (end_line - start_line + 1) >= ?`
	args := []any{opts.MinLines}
	if opts.PathPrefix != "" {
		q += ` AND path LIKE ?`
		args = append(args, opts.PathPrefix+"%")
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, opts.MaxCandidates)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clone candidates: %w", err)
	}
	cands := make(map[int64]cloneCand)
	order := make([]int64, 0, 128)
	for rows.Next() {
		var id int64
		var c cloneCand
		if err := rows.Scan(&id, &c.path, &c.kind, &c.name, &c.start, &c.end, &c.vec); err != nil {
			rows.Close()
			return nil, err
		}
		if len(c.vec)%4 != 0 {
			continue
		}
		cands[id] = c
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(order) < 2 {
		return nil, nil
	}

	// 2. Per-candidate KNN → deduped undirected duplicate edges.
	knn := fmt.Sprintf(`SELECT rowid, distance FROM chunk_vecs
	 WHERE embedding MATCH %s AND k = ?
	 ORDER BY distance`, s.vecMatchExpr())
	type pair struct{ a, b int64 }
	edges := make(map[pair]float32)
	for _, id := range order {
		c := cands[id]
		nr, err := s.db.QueryContext(ctx, knn, c.vec, opts.K+1)
		if err != nil {
			return nil, fmt.Errorf("clone knn: %w", err)
		}
		for nr.Next() {
			var nid int64
			var dist float64
			if err := nr.Scan(&nid, &dist); err != nil {
				nr.Close()
				return nil, err
			}
			if nid == id {
				continue
			}
			// Rows arrive by ascending distance (descending similarity), so
			// once we drop below the threshold every later row is worse too.
			sim := float32(1 - dist)
			if sim < opts.Threshold {
				break
			}
			other, ok := cands[nid]
			if !ok {
				continue // neighbour isn't a candidate (too short / filtered)
			}
			// Same file with overlapping spans = the same code, not a clone.
			if other.path == c.path && rangesOverlap(c.start, c.end, other.start, other.end) {
				continue
			}
			a, b := id, nid
			if a > b {
				a, b = b, a
			}
			p := pair{a, b}
			if prev, seen := edges[p]; !seen || sim < prev {
				edges[p] = sim
			}
		}
		if err := nr.Err(); err != nil {
			nr.Close()
			return nil, err
		}
		nr.Close()
	}
	if len(edges) == 0 {
		return nil, nil
	}

	// 3. Union-find the edges into connected components.
	uf := newCloneUF()
	for p := range edges {
		uf.union(p.a, p.b)
	}
	// 4. Group members per component and floor each cluster's similarity at its
	// weakest edge.
	members := make(map[int64][]int64) // root -> member ids
	seen := make(map[int64]bool)
	minSim := make(map[int64]float32) // root -> weakest edge sim
	for p, sim := range edges {
		root := uf.find(p.a)
		for _, id := range [2]int64{p.a, p.b} {
			if !seen[id] {
				seen[id] = true
				members[root] = append(members[root], id)
			}
		}
		if cur, ok := minSim[root]; !ok || sim < cur {
			minSim[root] = sim
		}
	}

	clusters := make([]CloneCluster, 0, len(members))
	for root, ids := range members {
		if len(ids) < 2 {
			continue
		}
		cl := CloneCluster{Similarity: minSim[root]}
		for _, id := range ids {
			c := cands[id]
			cl.Members = append(cl.Members, CloneMember{
				Path: c.path, StartLine: c.start, EndLine: c.end, Kind: c.kind, Name: c.name,
			})
		}
		sort.Slice(cl.Members, func(i, j int) bool {
			if cl.Members[i].Path != cl.Members[j].Path {
				return cl.Members[i].Path < cl.Members[j].Path
			}
			return cl.Members[i].StartLine < cl.Members[j].StartLine
		})
		clusters = append(clusters, cl)
	}
	// Biggest, then tightest, first.
	sort.Slice(clusters, func(i, j int) bool {
		if len(clusters[i].Members) != len(clusters[j].Members) {
			return len(clusters[i].Members) > len(clusters[j].Members)
		}
		return clusters[i].Similarity > clusters[j].Similarity
	})
	if len(clusters) > opts.MaxClusters {
		clusters = clusters[:opts.MaxClusters]
	}
	return clusters, nil
}

// rangesOverlap reports whether two inclusive line spans intersect.
func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd && bStart <= aEnd
}

// cloneUF is a tiny map-backed union-find over chunk ids.
type cloneUF struct{ parent map[int64]int64 }

func newCloneUF() *cloneUF { return &cloneUF{parent: make(map[int64]int64)} }

func (u *cloneUF) find(x int64) int64 {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		return x
	}
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]] // path halving
		x = u.parent[x]
	}
	return x
}

func (u *cloneUF) union(a, b int64) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}
