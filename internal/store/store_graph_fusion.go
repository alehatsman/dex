package store

// Graph-RAG retrieval fusion (#651, extracted from store_graph.go for
// navigability). The graph-augmented retrieval subsystem: spreading activation
// over the file-edge graph, graph-neighbor expansion, and fusion of those
// graph-proximity signals into the primary hit list. Methods are on *Store and
// share the same *sql.DB and migrations as the rest of the store; this file is
// a pure cohesion boundary, no behaviour change.

import (
	"context"
	"database/sql"
	"sort"
)

// GraphNeighborFiles returns file paths that are graph-adjacent (1-hop via any
// edge kind) to any of the seed files. Seed files are excluded from results.
// Results are ordered by PageRank so the most architecturally central
// neighbors rank first. Used by the search layer as a graph-proximity RRF lane.
func (s *Store) GraphNeighborFiles(ctx context.Context, seeds []string, limit int) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	seedSet := make(map[string]struct{}, len(seeds))
	for _, p := range seeds {
		seedSet[p] = struct{}{}
	}
	args := make([]any, len(seeds)+1)
	for i, p := range seeds {
		args[i] = p
	}
	args[len(seeds)] = limit * 3 // over-fetch; seeds filtered client-side

	rows, err := s.db.QueryContext(ctx, `
		WITH seed_nodes AS (
		  SELECT id FROM graph_nodes WHERE file_path IN (`+inPlaceholders(len(seeds))+`)
		),
		neighbor_ids AS (
		  SELECT dst_id AS id FROM graph_edges WHERE src_id IN (SELECT id FROM seed_nodes)
		  UNION
		  SELECT src_id AS id FROM graph_edges WHERE dst_id IN (SELECT id FROM seed_nodes)
		)
		SELECT DISTINCT gn.file_path
		FROM graph_nodes gn
		JOIN neighbor_ids ni ON gn.id = ni.id
		WHERE gn.file_path != ''
		ORDER BY gn.pagerank DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		if _, isSeed := seedSet[fp]; isSeed {
			continue
		}
		out = append(out, fp)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// HitsForFiles returns chunks for the given file paths ordered by graph
// PageRank descending, so the most architecturally central chunks come first.
// Used as input for the graph-proximity RRF lane in search fusion.
func (s *Store) HitsForFiles(ctx context.Context, paths []string, k int) ([]Hit, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 30
	}
	const batchSize = 500
	var out []Hit
	for i := 0; i < len(paths) && len(out) < k; i += batchSize {
		end := min(i+batchSize, len(paths))
		slice := paths[i:end]
		want := k - len(out)
		args := make([]any, len(slice)+1)
		for j, p := range slice {
			args[j] = p
		}
		args[len(slice)] = want

		rows, err := s.db.QueryContext(ctx, `
			SELECT c.path, c.kind, c.name, c.start_line, c.end_line,
			       COALESCE(gn.pagerank, 0) AS pr, c.content
			FROM chunks c
			LEFT JOIN graph_nodes gn ON gn.chunk_id = c.id
			WHERE c.path IN (`+inPlaceholders(len(slice))+`)
			ORDER BY pr DESC
			LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		out, err = scanFileHits(rows, out)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanFileHits drains a chunks+graph_nodes query into Hit rows, appending to
// out, and surfaces scan/iteration errors instead of swallowing them. It
// closes rows.
func scanFileHits(rows *sql.Rows, out []Hit) ([]Hit, error) {
	defer rows.Close()
	for rows.Next() {
		var h Hit
		var pr float64
		if err := rows.Scan(&h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &pr, &h.Content); err != nil {
			return out, err
		}
		h.Score = float32(pr)
		out = append(out, h)
	}
	return out, rows.Err()
}

// defaultGraphGamma is the per-hop decay for the graph-proximity lane when
// Options.GraphGamma is unset. γ=0.6 makes a 1-hop neighbor (0.60) outweigh
// the old flat 0.5× lane while a 3-hop neighbor (0.22) is strongly damped —
// tuned on the retrieval eval harness (#248). Sourced from the embedded
// calibration artifact (calibration.yml / #467).
var defaultGraphGamma = CalibratedDefaults().GraphGamma

// defaultGraphLaneWeight is the flat multiplier on the graph-proximity lane
// when Options.GraphLaneWeight is unset. 1.0 = neutral (lane contribution
// equals γ^hop, ≤0.6 of a primary hit at the same rank). Raise to make
// the graph lane compete more strongly with dense+BM25 — see DEX_GRAPH_WEIGHT.
// Sourced from the embedded calibration artifact (calibration.yml / #467).
var defaultGraphLaneWeight = CalibratedDefaults().GraphLaneWeight

// fuseWithGraphNeighbors merges primary hits with graph-proximity hits via
// Reciprocal Rank Fusion (k=60). Each graph hit is weighted by
// laneWeight×γ^hop (via weightByPath, keyed on file path) so 1-hop structural
// neighbors boost more than distant ones. laneWeight scales the whole lane
// independently of hop decay — raise it to make the graph lane compete with
// dense+BM25. A path absent from weightByPath gets zero contribution rather
// than a panic.
//
// Both legs are scored purely from rank position (1/(kRRF+i+1)); the incoming
// Hit.Score magnitude is discarded. This makes the lane fusion FUSION-MODE
// INDEPENDENT: whether the primary hits arrived from FusionRRF (~1/60 scores)
// or FusionLinear ([0,1] scores), only their ORDER feeds this stage, so the
// graph lane is never "under-weighted" in linear mode and laneWeight needs no
// per-mode rescaling. Do not add an Hit.Score-magnitude term here without
// renormalizing per mode — TestFuseWithGraphNeighborsRankBasedModeInvariant
// locks this property.
func fuseWithGraphNeighbors(primary, graphHits []Hit, weightByPath map[string]float32, laneWeight float32, n int) []Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(primary)+len(graphHits))
	byKey := make(map[hitKey]Hit, len(primary)+len(graphHits))
	fromPrimary := make(map[hitKey]struct{}, len(primary))

	for i, h := range primary {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
		fromPrimary[hk] = struct{}{}
	}
	for i, h := range graphHits {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += laneWeight * weightByPath[h.Path] / float32(kRRF+i+1)
		if existing, exists := byKey[hk]; exists {
			// A primary hit that is also a graph neighbor — union the graph
			// lane onto the kept (primary) representative for provenance (#707).
			existing.Lanes = existing.Lanes.With(LaneGraph)
			byKey[hk] = existing
		} else {
			h.Lanes = h.Lanes.With(LaneGraph)
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}
	out := make([]Hit, len(all))
	for i, r := range all {
		out[i] = byKey[r.key]
		// Graph-only neighbors skip the downstream reranker (held out as a
		// breadth-only tail), so stamp the fused score they're sorted by as
		// SortScore — otherwise they'd fall back to a near-zero cosine and
		// invert the rendered score= at the tail (#518). Primary hits are
		// left alone; their SortScore comes from the rerank pass.
		if _, ok := fromPrimary[r.key]; !ok {
			out[i].SortScore = r.score
		}
	}
	return out
}

// SeedFile is a file with an initial activation weight for SpreadActivation.
type SeedFile struct {
	Path   string
	Weight float32
}

// fileEdge is one entry from fileEdgesBidirectional.
type fileEdge struct {
	srcFile string
	dstFile string
	outDeg  int // distinct neighbor files for srcFile (for fan-out normalization)
}

// fileEdgesBidirectional returns file-level edges (both directions) for the
// given source files, along with each source file's distinct out-degree.
func (s *Store) fileEdgesBidirectional(ctx context.Context, files []string) ([]fileEdge, error) {
	if len(files) == 0 {
		return nil, nil
	}
	args := make([]any, len(files))
	for i, f := range files {
		args[i] = f
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH active_nodes AS (
		  SELECT id, file_path FROM graph_nodes
		  WHERE file_path IN (`+inPlaceholders(len(files))+`) AND file_path != ''
		),
		fwd AS (
		  SELECT an.file_path AS src_file, gn.file_path AS dst_file
		  FROM graph_edges ge
		  JOIN active_nodes an ON an.id = ge.src_id
		  JOIN graph_nodes gn ON gn.id = ge.dst_id
		  WHERE gn.file_path != '' AND gn.file_path != an.file_path
		),
		rev AS (
		  SELECT an.file_path AS src_file, gn.file_path AS dst_file
		  FROM graph_edges ge
		  JOIN active_nodes an ON an.id = ge.dst_id
		  JOIN graph_nodes gn ON gn.id = ge.src_id
		  WHERE gn.file_path != '' AND gn.file_path != an.file_path
		),
		all_edges AS (
		  SELECT src_file, dst_file FROM fwd
		  UNION
		  SELECT src_file, dst_file FROM rev
		),
		out_degrees AS (
		  SELECT src_file, COUNT(*) AS out_deg FROM all_edges GROUP BY src_file
		)
		SELECT ae.src_file, ae.dst_file, od.out_deg
		FROM all_edges ae
		JOIN out_degrees od ON od.src_file = ae.src_file`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileEdge
	for rows.Next() {
		var e fileEdge
		if err := rows.Scan(&e.srcFile, &e.dstFile, &e.outDeg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActivatedFile is one non-seed file surfaced by spreading activation, with
// its accumulated energy and the hop distance at which it was first reached
// (1-hop = direct neighbor of a seed). Hop drives the γ^hop fusion weight.
type ActivatedFile struct {
	Path   string
	Energy float32
	Hop    int
}

// defaultGraphHopCap bounds spreading-activation traversal depth. Sourced from
// the embedded calibration artifact (calibration.yml / #467).
var defaultGraphHopCap = CalibratedDefaults().GraphHopCap

// hopCap returns the configured spreading-activation depth, defaulting to
// defaultGraphHopCap when unset.
func (s *Store) hopCap() int {
	if s.opts.GraphHopCap > 0 {
		return s.opts.GraphHopCap
	}
	return defaultGraphHopCap
}

// SpreadActivation runs spreading activation over the file-level call graph
// and returns the top-n non-seed file paths by accumulated activation. It is
// a thin wrapper over spreadActivation for callers that only need the paths.
func (s *Store) SpreadActivation(ctx context.Context, seeds []SeedFile, n int) ([]string, error) {
	activated, err := s.spreadActivation(ctx, seeds, n)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(activated))
	for i, a := range activated {
		out[i] = a.Path
	}
	return out, nil
}

// spreadActivation runs spreading activation over the file-level call graph.
// Seeds carry initial activation weights (typically proportional to RRF scores).
// Energy spreads bidirectionally along graph_edges with fan-out normalization
// (each unit of energy distributes equally across all connected files) and
// per-hop decay (0.7). Pulses below threshold (1e-4) are pruned. Traversal
// stops after hopCap() iterations. Each file records the hop at which it was
// first reached. The top-n non-seed files by accumulated activation are
// returned, carrying that hop distance for γ^hop fusion weighting.
func (s *Store) spreadActivation(ctx context.Context, seeds []SeedFile, n int) ([]ActivatedFile, error) {
	const (
		decay     = float32(0.7)
		threshold = float32(1e-4)
	)
	maxHops := s.hopCap()
	if len(seeds) == 0 || n <= 0 {
		return nil, nil
	}
	seedSet := make(map[string]struct{}, len(seeds))
	activation := make(map[string]float32, len(seeds)*8)
	hopOf := make(map[string]int, len(seeds)*8)
	for _, sf := range seeds {
		seedSet[sf.Path] = struct{}{}
		activation[sf.Path] = sf.Weight
		hopOf[sf.Path] = 0
	}

	for hop := 1; hop <= maxHops; hop++ {
		var active []string
		for path, energy := range activation {
			if energy > threshold {
				active = append(active, path)
			}
		}
		if len(active) == 0 {
			break
		}
		edges, err := s.fileEdgesBidirectional(ctx, active)
		if err != nil {
			return nil, err
		}
		if len(edges) == 0 {
			break
		}
		// Spread using snapshot of current activation to ensure parallel semantics.
		snapshot := make(map[string]float32, len(activation))
		for p, e := range activation {
			snapshot[p] = e
		}
		for _, e := range edges {
			energy := snapshot[e.srcFile]
			if energy <= threshold || e.outDeg == 0 {
				continue
			}
			activation[e.dstFile] += energy * decay / float32(e.outDeg)
			// Record the first (shortest) hop at which this file lit up.
			if _, seen := hopOf[e.dstFile]; !seen {
				hopOf[e.dstFile] = hop
			}
		}
	}

	var results []ActivatedFile
	for path, energy := range activation {
		if _, isSeed := seedSet[path]; isSeed {
			continue
		}
		if energy > threshold {
			results = append(results, ActivatedFile{Path: path, Energy: energy, Hop: hopOf[path]})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Energy > results[j].Energy
	})
	if len(results) > n {
		results = results[:n]
	}
	return results, nil
}

// activationSeeds builds the spreading-activation seed set: session-recent
// files (weight 1.0) blended with their co-access neighbors (0.8×), then the
// primary semantic hits weighted proportional to score. Dedup by path, first
// write wins. Extracted from FuseSpreadingActivation to keep that method under
// the cyclomatic cap once the graph-off guard (#470) is added.
func (s *Store) activationSeeds(ctx context.Context, hits []Hit) []SeedFile {
	seeds := make([]SeedFile, 0, 16)
	seen := make(map[string]struct{}, 16)

	if ss, ok, err := s.SessionGet(ctx); err == nil && ok {
		for _, f := range ss.Files {
			if _, dup := seen[f.Path]; !dup {
				seeds = append(seeds, SeedFile{Path: f.Path, Weight: 1.0})
				seen[f.Path] = struct{}{}
			}
		}
		// Blend co-access neighbors of the session working set at 0.8×.
		// These represent files that have historically been read alongside the
		// current working set and are likely relevant even if not semantic hits.
		if sessionPaths := make([]string, 0, len(ss.Files)); len(ss.Files) > 0 {
			for _, f := range ss.Files {
				sessionPaths = append(sessionPaths, f.Path)
			}
			if neighbors, err := s.CoAccessNeighbors(ctx, sessionPaths, 8); err == nil {
				for _, p := range neighbors {
					if _, dup := seen[p]; !dup {
						seeds = append(seeds, SeedFile{Path: p, Weight: 0.8})
						seen[p] = struct{}{}
					}
				}
			}
		}
	}

	// Primary hits seed BFS (structural discovery from what's already relevant).
	// GraphSeedTopN (spike #225) gates this to the top N by score instead of
	// every hit in the fused pool — session-recent files and their co-access
	// neighbors above are never gated, only this blanket-by-default leg.
	primary := hits
	if s.opts.GraphSeedTopN > 0 && len(hits) > s.opts.GraphSeedTopN {
		ranked := make([]Hit, len(hits))
		copy(ranked, hits)
		sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
		primary = ranked[:s.opts.GraphSeedTopN]
	}
	var maxScore float32
	for _, h := range primary {
		if h.Score > maxScore {
			maxScore = h.Score
		}
	}
	if maxScore <= 0 {
		maxScore = 1
	}
	for _, h := range primary {
		if _, dup := seen[h.Path]; !dup {
			seeds = append(seeds, SeedFile{Path: h.Path, Weight: h.Score / maxScore})
			seen[h.Path] = struct{}{}
		}
	}
	return seeds
}

// FuseSpreadingActivation expands the hit set using spreading activation.
// When queryVec is non-nil and node_vecs is populated, seeds come from the
// top-k symbol KNN matches for the query vector (query→symbol→BFS). This
// finds structurally-coupled files regardless of whether primary semantic
// hits include the target. Falls back to primary-hit file seeds when
// node_vecs is empty or KNN returns nothing. Silently returns primary hits
// unchanged on any store failure — graph proximity is best-effort.
func (s *Store) FuseSpreadingActivation(ctx context.Context, hits []Hit, queryVec []float32, n int) []Hit {
	if len(hits) == 0 {
		return hits
	}
	// Lane held out (graph-off ablation, #470): return the primary hits
	// unchanged. This is the true "lane off" the weight can't express —
	// GraphLaneWeight = 0 is the "unset → use default 1.0" sentinel.
	if s.opts.GraphLaneDisabled {
		return hits
	}

	seeds := s.activationSeeds(ctx, hits)

	activated, err := s.spreadActivation(ctx, seeds, 15)
	if err != nil {
		return hits
	}

	// Weight each activated file by γ^hop so 1-hop structural neighbors boost
	// more than distant ones.
	gamma := s.opts.GraphGamma
	if gamma <= 0 {
		gamma = defaultGraphGamma
	}
	laneWeight := s.opts.GraphLaneWeight
	if laneWeight <= 0 {
		laneWeight = defaultGraphLaneWeight
	}

	// BFS graph hits.
	paths := make([]string, len(activated))
	weightByPath := make(map[string]float32, len(activated))
	for i, a := range activated {
		paths[i] = a.Path
		hop := a.Hop
		if hop < 1 {
			hop = 1
		}
		weightByPath[a.Path] = pow32(gamma, hop)
	}
	var graphHits []Hit
	if len(paths) > 0 {
		graphHits, err = s.HitsForFiles(ctx, paths, n*2)
		if err != nil {
			return hits
		}
	}

	// Symbol KNN structural discovery: find the closest symbol nodes to the
	// query vector and fetch chunks for files that CALL INTO those nodes.
	// Using callers (not the nodes themselves) avoids activating definition-side
	// dependencies that hurt orphan recall. For orphan queries, callers of the
	// KNN-found definition are the exact import-site targets. For structural
	// queries, callers of the KNN-found coupled file are the co-coupling targets.
	if len(queryVec) > 0 {
		if nvCount, err2 := s.NodeVecCount(ctx); err2 == nil && nvCount > 0 {
			if _, knnFiles, _, err2 := s.NodeKNN(ctx, queryVec, 3); err2 == nil {
				var knnSeedFiles []string
				for _, fp := range knnFiles {
					if fp != "" {
						knnSeedFiles = append(knnSeedFiles, fp)
					}
				}
				if callers, err2 := s.CallerFiles(ctx, knnSeedFiles, 20); err2 == nil && len(callers) > 0 {
					callerHits, err2 := s.HitsForFiles(ctx, callers, n)
					if err2 == nil {
						for _, h := range callerHits {
							if _, ok := weightByPath[h.Path]; !ok {
								// treat as 1-hop neighbor
								weightByPath[h.Path] = pow32(gamma, 1)
							}
						}
						graphHits = append(graphHits, callerHits...)
					}
				}
			}
		}
	}

	if len(graphHits) == 0 {
		return hits
	}
	return fuseWithGraphNeighbors(hits, graphHits, weightByPath, laneWeight, n)
}

// pow32 returns base^exp for a small non-negative integer exponent.
func pow32(base float32, exp int) float32 {
	out := float32(1)
	for range exp {
		out *= base
	}
	return out
}
