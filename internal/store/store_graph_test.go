package store

import (
	"testing"
	"time"
)

func funcNode(id, name, pkg, file string) GraphNodeRow {
	return GraphNodeRow{
		ID: id, Kind: "function", Name: name,
		PackagePath: pkg, FilePath: file,
		StartLine: 1, EndLine: 10, ContentHash: id + "-h",
	}
}

func importNode(id, importer, importee string) GraphNodeRow {
	return GraphNodeRow{
		ID: id, Kind: "import", Name: importee,
		PackagePath: importer, FilePath: "",
		ContentHash: id + "-h",
	}
}

func callEdge(id, src, dst, file string) GraphEdgeRow {
	return GraphEdgeRow{
		ID: id, Kind: "calls", SrcID: src, DstID: dst,
		FilePath: file, ContentHash: id + "-h",
	}
}

func TestGraphUpsertAndStats(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Foo", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "Bar", "pkg/b", "pkg/b/b.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		callEdge("e1", "n1", "n2", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	gotNodes, gotEdges, err := st.GraphStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotNodes != 2 {
		t.Errorf("nodes=%d, want 2", gotNodes)
	}
	if gotEdges != 1 {
		t.Errorf("edges=%d, want 1", gotEdges)
	}
}

func TestGraphAllNodesAndEdges(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Foo", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "Bar", "pkg/a", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		callEdge("e1", "n1", "n2", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	allNodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(allNodes) != 2 {
		t.Fatalf("allNodes=%d, want 2", len(allNodes))
	}
	allEdges, err := st.GraphAllEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(allEdges) != 1 {
		t.Fatalf("allEdges=%d, want 1", len(allEdges))
	}
	if allEdges[0].Kind != "calls" {
		t.Errorf("edge kind=%s, want calls", allEdges[0].Kind)
	}
}

func TestGraphSetCentralityAndTopCentral(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Foo", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "Bar", "pkg/a", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphSetCentrality(ctx, []GraphCentralityRow{
		{ID: "n1", PageRank: 0.9, InDegree: 3},
		{ID: "n2", PageRank: 0.1, InDegree: 1},
	}); err != nil {
		t.Fatal(err)
	}

	top, err := st.TopCentralByDir(ctx, "pkg/a", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) < 2 {
		t.Fatalf("top=%d, want >=2", len(top))
	}
	if top[0].Name != "Foo" {
		t.Errorf("top[0]=%s, want Foo (highest pagerank)", top[0].Name)
	}
}

func TestGraphPruneUnseen(t *testing.T) {
	st, ctx := newStore(t)
	old := time.Now().Add(-24 * time.Hour)
	recent := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{funcNode("old", "Old", "pkg/a", "pkg/a/a.go")}, old); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{funcNode("new", "New", "pkg/a", "pkg/a/a.go")}, recent); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-time.Hour)
	prunedN, _, err := st.GraphPruneUnseen(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if prunedN != 1 {
		t.Errorf("pruned=%d, want 1", prunedN)
	}
	n, _, _ := st.GraphStats(ctx)
	if n != 1 {
		t.Errorf("remaining nodes=%d, want 1", n)
	}
}

func TestChunksByPaths(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a/a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h1", Content: "func A(){}"},
		{Path: "a/a.go", Kind: "fn", StartLine: 6, EndLine: 10, ContentSHA: "h2", Content: "func B(){}"},
		{Path: "b/b.go", Kind: "fn", StartLine: 1, EndLine: 4, ContentSHA: "h3", Content: "func C(){}"},
	}, now); err != nil {
		t.Fatal(err)
	}

	result, err := st.ChunksByPaths(ctx, []string{"a/a.go", "b/b.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result["a/a.go"]) != 2 {
		t.Errorf("a/a.go chunks=%d, want 2", len(result["a/a.go"]))
	}
	if len(result["b/b.go"]) != 1 {
		t.Errorf("b/b.go chunks=%d, want 1", len(result["b/b.go"]))
	}
}

func TestSymbolsByFile(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Foo", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "Bar", "pkg/a", "pkg/a/a.go"),
		funcNode("n3", "Baz", "pkg/b", "pkg/b/b.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	syms, err := st.SymbolsByFile(ctx, "pkg/a/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 2 {
		t.Errorf("symbols=%d, want 2", len(syms))
	}
}

func TestExportedSymbolsByDir(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Exported", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "unexported", "pkg/a", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	syms, err := st.ExportedSymbolsByDir(ctx, "pkg/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 {
		t.Fatalf("exported=%d, want 1", len(syms))
	}
	if syms[0].Name != "Exported" {
		t.Errorf("name=%s, want Exported", syms[0].Name)
	}
}

func TestImportsForDirAndFile(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// Regular node to anchor pkg/a to a file (needed by the CTE).
	// Import node (kind=import, file_path="") records that pkg/a imports pkg/b.
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("fn1", "Fn", "pkg/a", "pkg/a/a.go"),
		importNode("imp1", "pkg/a", "pkg/b"),
	}, now); err != nil {
		t.Fatal(err)
	}

	pkgs, err := st.ImportsForDir(ctx, "pkg/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0] != "pkg/b" {
		t.Errorf("ImportsForDir=%v, want [pkg/b]", pkgs)
	}

	pkgsFile, err := st.ImportsForFile(ctx, "pkg/a/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgsFile) != 1 || pkgsFile[0] != "pkg/b" {
		t.Errorf("ImportsForFile=%v, want [pkg/b]", pkgsFile)
	}
}

func TestUsedByPackages(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// pkg/a imports pkg/b; so pkg/b is used by pkg/a.
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("fn1", "Fn", "pkg/a", "pkg/a/a.go"),
		funcNode("fn2", "Fn", "pkg/b", "pkg/b/b.go"),
		importNode("imp1", "pkg/a", "pkg/b"),
	}, now); err != nil {
		t.Fatal(err)
	}

	callers, err := st.UsedByPackages(ctx, "pkg/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0] != "pkg/a" {
		t.Errorf("UsedByPackages=%v, want [pkg/a]", callers)
	}
}

func TestGraphNeighborFiles(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "Foo", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "Bar", "pkg/b", "pkg/b/b.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		callEdge("e1", "n1", "n2", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	neighbors, err := st.GraphNeighborFiles(ctx, []string{"pkg/a/a.go"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range neighbors {
		if n == "pkg/b/b.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("neighbors=%v, want to include pkg/b/b.go", neighbors)
	}
}

func TestHitsForFiles(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.UpsertMany(ctx, []PendingChunk{
		{Path: "a/a.go", Kind: "fn", StartLine: 1, EndLine: 5, ContentSHA: "h1", Content: "func Foo(){}"},
	}, now); err != nil {
		t.Fatal(err)
	}

	hits, err := st.HitsForFiles(ctx, []string{"a/a.go"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits=%d, want 1", len(hits))
	}
	if hits[0].Path != "a/a.go" {
		t.Errorf("path=%s, want a/a.go", hits[0].Path)
	}
}

func TestCallerFiles(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("src", "Src", "pkg/a", "pkg/a/a.go"),
		funcNode("dst", "Dst", "pkg/b", "pkg/b/b.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphUpsertEdges(ctx, []GraphEdgeRow{
		callEdge("e1", "src", "dst", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}

	callers, err := st.CallerFiles(ctx, []string{"pkg/b/b.go"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0] != "pkg/a/a.go" {
		t.Errorf("CallerFiles=%v, want [pkg/a/a.go]", callers)
	}
}

func TestGraphSeenTime(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	// On empty graph: returns the `now` argument (no rows → MAX(last_seen_at) is NULL → default to now.UnixNano()).
	ts, err := st.GraphSeenTime(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if ts.UnixNano() != now.UnixNano() {
		t.Errorf("empty graph seen time=%v, want now", ts)
	}

	later := now.Add(time.Minute)
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{funcNode("n1", "F", "pkg", "pkg/a.go")}, later); err != nil {
		t.Fatal(err)
	}

	ts2, err := st.GraphSeenTime(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if ts2.Before(later.Truncate(time.Microsecond)) {
		t.Errorf("seen time=%v, want >= %v", ts2, later)
	}
}

func TestFileCentrality(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "F", "pkg/a", "pkg/a/a.go"),
		funcNode("n2", "G", "pkg/a", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphSetCentrality(ctx, []GraphCentralityRow{
		{ID: "n1", PageRank: 0.6},
		{ID: "n2", PageRank: 0.4},
	}); err != nil {
		t.Fatal(err)
	}

	fc, err := st.FileCentrality(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const want = 1.0
	if fc["pkg/a/a.go"] < 0.99 || fc["pkg/a/a.go"] > 1.01 {
		t.Errorf("FileCentrality[pkg/a/a.go]=%v, want ~%v", fc["pkg/a/a.go"], want)
	}
}

func TestPackageCentrality(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
		funcNode("n1", "F", "pkg/a", "pkg/a/a.go"),
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.GraphSetCentrality(ctx, []GraphCentralityRow{{ID: "n1", PageRank: 0.5}}); err != nil {
		t.Fatal(err)
	}

	pc, err := st.PackageCentrality(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pc["pkg/a"] < 0.49 || pc["pkg/a"] > 0.51 {
		t.Errorf("PackageCentrality[pkg/a]=%v, want ~0.5", pc["pkg/a"])
	}
}
