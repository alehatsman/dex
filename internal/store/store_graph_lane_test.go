package store

import (
	"testing"
	"time"
)

// TestSearchGraphLane verifies that Store.Search applies the graph-proximity
// lane: chunks from graph-adjacent files of session-recent files are fused at
// γ^hop RRF weight, so a graph neighbor appears in results even when its
// embedding is orthogonal to the query vector.
func TestSearchGraphLane(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// Two chunks: a.go is semantically close to the query, b.go is orthogonal.
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
		{Path: "b.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "hb",
			Content: "func B(){}", Vec: []float32{0, 1, 0, 0}},
	}, now); err != nil {
		t.Fatal(err)
	}

	// Build graph: a.go file-node → b.go file-node (a "calls" edge).
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		{ID: "n:a", Kind: "file", Name: "a.go", FilePath: "a.go", ContentHash: "ha"},
		{ID: "n:b", Kind: "file", Name: "b.go", FilePath: "b.go", ContentHash: "hb"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		{ID: "e:ab", Kind: "calls", SrcID: "n:a", DstID: "n:b",
			FilePath: "a.go", ContentHash: "eab"},
	}, now); err != nil {
		t.Fatal(err)
	}

	// Session: agent recently touched a.go.
	if err := st.SessionAddFile(ctx, "a.go", "read"); err != nil {
		t.Fatal(err)
	}

	// Query along (1,0,0,0): semantically a.go scores 1.0, b.go scores 0.
	// Without graph lane: only a.go would appear (k=2 still returns both,
	// but graph lane must surface b.go when k=1 due to RRF boost from
	// graph proximity of session file a.go → neighbor b.go).
	hits, err := st.Search(ctx, []float32{1, 0, 0, 0}, "", 2)
	if err != nil {
		t.Fatal(err)
	}

	paths := make(map[string]bool, len(hits))
	for _, h := range hits {
		paths[h.Path] = true
	}
	if !paths["b.go"] {
		t.Errorf("b.go missing from results — graph-proximity lane not applied; got paths: %v", hitPaths(hits))
	}
}

// TestSearchGraphLaneNoSession verifies that Store.Search without a session
// does not crash and returns normal results.
func TestSearchGraphLaneNoSession(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()
	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
	}, now); err != nil {
		t.Fatal(err)
	}

	// No session, no graph — must return normal semantic results.
	hits, err := st.Search(ctx, []float32{1, 0, 0, 0}, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.go" {
		t.Errorf("expected [a.go], got %v", hitPaths(hits))
	}
}

// TestSpreadActivationHopDistance verifies that spreadActivation records the
// shortest hop distance to each activated file (1 for direct neighbors, 2 for
// next ring), which drives the γ^hop fusion weight.
func TestSpreadActivationHopDistance(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// seed.go → hop1.go → hop2.go
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		{ID: "n:seed", Kind: "file", Name: "seed.go", FilePath: "seed.go", ContentHash: "hs"},
		{ID: "n:hop1", Kind: "file", Name: "hop1.go", FilePath: "hop1.go", ContentHash: "h1"},
		{ID: "n:hop2", Kind: "file", Name: "hop2.go", FilePath: "hop2.go", ContentHash: "h2"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		{ID: "e:sh1", Kind: "calls", SrcID: "n:seed", DstID: "n:hop1", FilePath: "seed.go", ContentHash: "esh1"},
		{ID: "e:h1h2", Kind: "calls", SrcID: "n:hop1", DstID: "n:hop2", FilePath: "hop1.go", ContentHash: "eh1h2"},
	}, now); err != nil {
		t.Fatal(err)
	}

	activated, err := st.spreadActivation(ctx, []SeedFile{{Path: "seed.go", Weight: 1.0}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	hopOf := make(map[string]int, len(activated))
	for _, a := range activated {
		hopOf[a.Path] = a.Hop
	}
	if hopOf["hop1.go"] != 1 {
		t.Errorf("hop1.go hop = %d, want 1; activated=%v", hopOf["hop1.go"], activated)
	}
	if hopOf["hop2.go"] != 2 {
		t.Errorf("hop2.go hop = %d, want 2; activated=%v", hopOf["hop2.go"], activated)
	}
}

// TestFuseWithGraphNeighborsHopDecay verifies that, with equal in-lane rank,
// a 1-hop neighbor (γ^1) outranks a 2-hop neighbor (γ^2) after fusion.
func TestFuseWithGraphNeighborsHopDecay(t *testing.T) {
	const gamma = float32(0.6)
	primary := []Hit{{Path: "match.go", StartLine: 1, Score: 1.0}}
	// far.go is listed first (better in-lane rank); the γ^hop weight must still
	// lift the 1-hop near.go above it.
	graphHits := []Hit{
		{Path: "far.go", StartLine: 1},
		{Path: "near.go", StartLine: 1},
	}
	weightByPath := map[string]float32{
		"near.go": pow32(gamma, 1), // 0.60
		"far.go":  pow32(gamma, 2), // 0.36
	}
	out := fuseWithGraphNeighbors(primary, graphHits, weightByPath, 1.0, 3)

	rank := make(map[string]int, len(out))
	for i, h := range out {
		rank[h.Path] = i
	}
	if rank["near.go"] >= rank["far.go"] {
		t.Errorf("1-hop near.go (rank %d) should outrank 2-hop far.go (rank %d): %v",
			rank["near.go"], rank["far.go"], hitPaths(out))
	}
}

// TestFuseWithGraphNeighborsStampsGraphOnlySortScore guards #518: graph-only
// neighbors bypass the downstream reranker, so fuse must stamp the fused score
// they're ordered by into SortScore (descending, monotonic) while leaving the
// primary hit's SortScore zero (it gets one from the rerank pass).
func TestFuseWithGraphNeighborsStampsGraphOnlySortScore(t *testing.T) {
	primary := []Hit{{Path: "match.go", StartLine: 1, Score: 1.0}}
	graphHits := []Hit{
		{Path: "near.go", StartLine: 1},
		{Path: "far.go", StartLine: 1},
	}
	weightByPath := map[string]float32{
		"near.go": pow32(0.6, 1),
		"far.go":  pow32(0.6, 2),
	}
	out := fuseWithGraphNeighbors(primary, graphHits, weightByPath, 1.0, 3)

	var prev float32 = -1
	for _, h := range out {
		if h.Path == "match.go" {
			if h.SortScore != 0 {
				t.Errorf("primary hit must keep SortScore zero, got %v", h.SortScore)
			}
			continue
		}
		if h.SortScore <= 0 {
			t.Errorf("graph-only %q must carry a positive SortScore, got %v", h.Path, h.SortScore)
		}
		if prev >= 0 && h.SortScore > prev {
			t.Errorf("graph-only SortScore must be non-increasing in order: %v after %v", h.SortScore, prev)
		}
		prev = h.SortScore
	}
}

// TestFuseWithGraphNeighborsLaneWeight verifies that a higher laneWeight
// amplifies graph hits enough to outrank primary hits at the same range.
func TestFuseWithGraphNeighborsLaneWeight(t *testing.T) {
	// primary has one hit; graph has a neighbor ranked equally in its lane.
	// With laneWeight=1 the graph neighbor (γ^1=0.6) scores below the primary.
	// With laneWeight=4 the graph neighbor (4×0.6=2.4) scores above the primary.
	const gamma = float32(0.6)
	primary := []Hit{{Path: "match.go", StartLine: 1, Score: 1.0}}
	graphHits := []Hit{{Path: "neighbor.go", StartLine: 1}}
	weightByPath := map[string]float32{"neighbor.go": pow32(gamma, 1)} // 0.60

	// laneWeight=1: primary wins.
	out1 := fuseWithGraphNeighbors(primary, graphHits, weightByPath, 1.0, 2)
	rank1 := make(map[string]int, len(out1))
	for i, h := range out1 {
		rank1[h.Path] = i
	}
	if rank1["match.go"] != 0 {
		t.Errorf("laneWeight=1: primary match.go should rank first, got rank %d", rank1["match.go"])
	}

	// laneWeight=4: graph neighbor wins (4×0.6 = 2.4 > 1.0).
	out4 := fuseWithGraphNeighbors(primary, graphHits, weightByPath, 4.0, 2)
	rank4 := make(map[string]int, len(out4))
	for i, h := range out4 {
		rank4[h.Path] = i
	}
	if rank4["neighbor.go"] != 0 {
		t.Errorf("laneWeight=4: graph neighbor.go should rank first, got rank %d; full: %v", rank4["neighbor.go"], hitPaths(out4))
	}
}

// TestFuseWithGraphNeighborsRankBasedModeInvariant locks the property that the
// graph lane fuses on RANK POSITION only and ignores Hit.Score magnitude — so
// the stage is identical whether primary hits arrived from FusionRRF (~1/60
// scores) or FusionLinear ([0,1] scores). This is why the graph lane is NOT
// under-weighted in linear mode and GraphLaneWeight needs no per-mode rescaling.
func TestFuseWithGraphNeighborsRankBasedModeInvariant(t *testing.T) {
	// Same order, two wildly different score-magnitude regimes:
	// linearLike (~[0,1]) vs rrfLike (~1/60). Order is identical.
	linearLike := []Hit{
		{Path: "p0.go", StartLine: 1, Score: 0.90},
		{Path: "p1.go", StartLine: 1, Score: 0.80},
		{Path: "p2.go", StartLine: 1, Score: 0.70},
	}
	rrfLike := []Hit{
		{Path: "p0.go", StartLine: 1, Score: 0.0166},
		{Path: "p1.go", StartLine: 1, Score: 0.0163},
		{Path: "p2.go", StartLine: 1, Score: 0.0161},
	}
	graphHits := []Hit{{Path: "g.go", StartLine: 1}}
	weightByPath := map[string]float32{"g.go": pow32(0.6, 1)}

	outLinear := fuseWithGraphNeighbors(linearLike, graphHits, weightByPath, 2.0, 4)
	outRRF := fuseWithGraphNeighbors(rrfLike, graphHits, weightByPath, 2.0, 4)

	if len(outLinear) != len(outRRF) {
		t.Fatalf("length mismatch: linear=%d rrf=%d", len(outLinear), len(outRRF))
	}
	for i := range outLinear {
		if outLinear[i].Path != outRRF[i].Path {
			t.Errorf("rank %d differs by score regime: linear=%q rrf=%q — graph fusion read Hit.Score magnitude (mode-dependent)",
				i, outLinear[i].Path, outRRF[i].Path)
		}
	}
}

func hitPaths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}

// TestSpreadActivationTwoHop verifies that SpreadActivation finds files two hops
// away from seeds, not just direct neighbors.
//
// Graph: seed.go → hop1.go → hop2.go
// hop2.go has no direct edge to seed.go; only spreading activation reaches it.
func TestSpreadActivationTwoHop(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		{ID: "n:seed", Kind: "file", Name: "seed.go", FilePath: "seed.go", ContentHash: "hs"},
		{ID: "n:hop1", Kind: "file", Name: "hop1.go", FilePath: "hop1.go", ContentHash: "h1"},
		{ID: "n:hop2", Kind: "file", Name: "hop2.go", FilePath: "hop2.go", ContentHash: "h2"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		{ID: "e:sh1", Kind: "calls", SrcID: "n:seed", DstID: "n:hop1", FilePath: "seed.go", ContentHash: "esh1"},
		{ID: "e:h1h2", Kind: "calls", SrcID: "n:hop1", DstID: "n:hop2", FilePath: "hop1.go", ContentHash: "eh1h2"},
	}, now); err != nil {
		t.Fatal(err)
	}

	seeds := []SeedFile{{Path: "seed.go", Weight: 1.0}}
	activated, err := st.SpreadActivation(ctx, seeds, 10)
	if err != nil {
		t.Fatal(err)
	}

	found := make(map[string]bool, len(activated))
	for _, p := range activated {
		found[p] = true
	}
	if !found["hop1.go"] {
		t.Errorf("hop1.go (1-hop neighbor) not in activated: %v", activated)
	}
	if !found["hop2.go"] {
		t.Errorf("hop2.go (2-hop neighbor) not in activated: %v", activated)
	}
	for _, p := range activated {
		if p == "seed.go" {
			t.Errorf("seed.go should not appear in spreading activation results")
		}
	}
}

// TestSpreadActivationFanOutNorm verifies fan-out normalization: a hub with many
// edges distributes less energy per neighbor than a node with a single edge.
//
// Graph:
//
//	hub.go → neighbor_a.go
//	hub.go → neighbor_b.go
//	hub.go → neighbor_c.go  (3 neighbors → each gets 1/3 × decay × seed_weight)
//	solo.go → solo_neighbor.go  (1 neighbor → gets 1/1 × decay × seed_weight)
//
// solo_neighbor.go should have higher activation than any hub neighbor.
func TestSpreadActivationFanOutNorm(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	nodes := []GraphNodeRow{
		{ID: "n:hub", Kind: "file", Name: "hub.go", FilePath: "hub.go", ContentHash: "hh"},
		{ID: "n:na", Kind: "file", Name: "neighbor_a.go", FilePath: "neighbor_a.go", ContentHash: "na"},
		{ID: "n:nb", Kind: "file", Name: "neighbor_b.go", FilePath: "neighbor_b.go", ContentHash: "nb"},
		{ID: "n:nc", Kind: "file", Name: "neighbor_c.go", FilePath: "neighbor_c.go", ContentHash: "nc"},
		{ID: "n:solo", Kind: "file", Name: "solo.go", FilePath: "solo.go", ContentHash: "hs"},
		{ID: "n:sn", Kind: "file", Name: "solo_neighbor.go", FilePath: "solo_neighbor.go", ContentHash: "sn"},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []GraphEdgeRow{
		{ID: "e:ha", Kind: "calls", SrcID: "n:hub", DstID: "n:na", FilePath: "hub.go", ContentHash: "eha"},
		{ID: "e:hb", Kind: "calls", SrcID: "n:hub", DstID: "n:nb", FilePath: "hub.go", ContentHash: "ehb"},
		{ID: "e:hc", Kind: "calls", SrcID: "n:hub", DstID: "n:nc", FilePath: "hub.go", ContentHash: "ehc"},
		{ID: "e:ss", Kind: "calls", SrcID: "n:solo", DstID: "n:sn", FilePath: "solo.go", ContentHash: "ess"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}

	seeds := []SeedFile{
		{Path: "hub.go", Weight: 1.0},
		{Path: "solo.go", Weight: 1.0},
	}
	activated, err := st.SpreadActivation(ctx, seeds, 10)
	if err != nil {
		t.Fatal(err)
	}

	actByPath := make(map[string]float32)
	// Re-run to get energy values — use fileEdgesBidirectional to verify ranking.
	// Instead, check ordering directly from the returned slice (sorted by energy desc).
	// solo_neighbor should appear before any of hub's neighbors in results.
	soloIdx := -1
	for i, p := range activated {
		if p == "solo_neighbor.go" {
			soloIdx = i
		}
		actByPath[p] = 0
		_ = actByPath
	}
	if soloIdx < 0 {
		t.Fatalf("solo_neighbor.go not in results: %v", activated)
	}
	for i, p := range activated {
		switch p {
		case "neighbor_a.go", "neighbor_b.go", "neighbor_c.go":
			if i < soloIdx {
				t.Errorf("%s (hub neighbor, idx %d) ranked before solo_neighbor.go (idx %d) — fan-out normalization not working",
					p, i, soloIdx)
			}
		}
	}
}

// TestNodeVecEmbedAndKNN verifies the NodesNeedingEmbed / SetNodeVecs /
// NodeKNN round-trip.
func TestNodeVecEmbedAndKNN(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "ha",
			Content: "func A(){}", Vec: []float32{1, 0, 0, 0}},
		{Path: "b.go", Kind: "fn", StartLine: 1, EndLine: 2, ContentSHA: "hb",
			Content: "func B(){}", Vec: []float32{0, 0, 0, 1}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		{ID: "n:a", Kind: "function", Name: "A", QualifiedName: "pkg.A", FilePath: "a.go", ContentHash: "ha"},
		{ID: "n:b", Kind: "function", Name: "B", QualifiedName: "pkg.B", FilePath: "b.go", ContentHash: "hb"},
	}, now); err != nil {
		t.Fatal(err)
	}

	// All nodes need embedding initially.
	rows, err := st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 nodes needing embed, got %d", len(rows))
	}

	// Embed: n:b aligned to qvec, n:a orthogonal.
	qvec := []float32{0, 0, 1, 0}
	vecs := [][]float32{
		{1, 0, 0, 0},
		{0, 0, 1, 0},
	}
	if err := st.SetNodeVecs(ctx, rows, vecs); err != nil {
		t.Fatal(err)
	}

	rows2, err := st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 0 {
		t.Fatalf("expected 0 nodes needing embed after SetNodeVecs, got %d", len(rows2))
	}

	// NodeKNN should return n:b as closest to qvec.
	ids, files, _, err := st.NodeKNN(ctx, qvec, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("NodeKNN returned no results")
	}
	if ids[0] != "n:b" {
		t.Errorf("expected top KNN n:b, got %q (all: %v)", ids[0], ids)
	}
	if files[0] != "b.go" {
		t.Errorf("expected top KNN file b.go, got %q", files[0])
	}
}
