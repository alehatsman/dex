package graph

import (
	"sort"
	"testing"

	"github.com/alehatsman/dex/internal/gitlog"
)

func fileNode(id, path string) Node {
	return Node{ID: id, Kind: NodeFile, FilePath: path}
}

// TestCoChangeSupportConfidence checks the pair-counting/support/confidence
// arithmetic against hand-computed values, including that commitsTouching
// increments once per file per commit (not once per pair).
func TestCoChangeSupportConfidence(t *testing.T) {
	commits := []gitlog.Commit{
		{ShortHash: "c1", Files: []string{"a.go", "b.go"}},
		{ShortHash: "c2", Files: []string{"a.go", "b.go"}},
		{ShortHash: "c3", Files: []string{"a.go", "c.go"}},
	}
	nodes := []Node{
		fileNode("fileA", "a.go"),
		fileNode("fileB", "b.go"),
		fileNode("fileC", "c.go"),
	}

	edges := coChangeEdgesFromCommits(commits, nodes)

	// a-b: support=2 (c1,c2), commitsTouching[a]=3, commitsTouching[b]=2 ->
	// confidence = 2/min(3,2) = 1.0. Passes support>=2 and confidence>=0.1.
	// a-c: support=1 -> dropped (below minSupport=2).
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.Kind != EdgeCoChanges {
		t.Errorf("Kind = %q, want %q", e.Kind, EdgeCoChanges)
	}
	support, _ := e.Metadata["support"].(float64)
	confidence, _ := e.Metadata["confidence"].(float64)
	if support != 2 {
		t.Errorf("support = %v, want 2", e.Metadata["support"])
	}
	if confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0", confidence)
	}
	// a < b lexicographically by node ID (fileA < fileB).
	if e.SrcID != "fileA" || e.DstID != "fileB" {
		t.Errorf("SrcID/DstID = %s/%s, want fileA/fileB", e.SrcID, e.DstID)
	}
}

// TestCoChangeConfidenceThreshold checks a pair that clears support but not
// confidence is dropped. Confidence is support / min(commitsTouching[A],
// commitsTouching[B]), so driving BOTH files' individual commit counts up
// (each mostly co-changing with an unrelated third file) dilutes confidence
// even though a-b's raw support stays fixed.
func TestCoChangeConfidenceThreshold(t *testing.T) {
	commits := []gitlog.Commit{
		{Files: []string{"a.go", "b.go"}}, // a-b support = 2
		{Files: []string{"a.go", "b.go"}},
	}
	for i := 0; i < 50; i++ {
		commits = append(commits, gitlog.Commit{Files: []string{"a.go", "y.go"}}) // commitsTouching[a] += 50
		commits = append(commits, gitlog.Commit{Files: []string{"b.go", "z.go"}}) // commitsTouching[b] += 50
	}
	// commitsTouching[a] = 52, commitsTouching[b] = 52 -> confidence =
	// 2/52 ≈ 0.038 < 0.1 threshold.
	nodes := []Node{
		fileNode("fileA", "a.go"), fileNode("fileB", "b.go"),
		fileNode("fileY", "y.go"), fileNode("fileZ", "z.go"),
	}

	edges := coChangeEdgesFromCommits(commits, nodes)
	for _, e := range edges {
		if (e.SrcID == "fileA" && e.DstID == "fileB") || (e.SrcID == "fileB" && e.DstID == "fileA") {
			t.Fatalf("a-b edge should be dropped by confidence threshold, got %+v", e)
		}
	}
}

// TestCoChangeDocumentNodeEndpoint checks a co_changes edge is emitted for a
// pair where one endpoint is a NodeDocument (markdown, ExtractMarkdown's
// file-level node kind) rather than NodeFile — e.g. a spec doc changing
// alongside its implementing source file, this repo's own specs/*.md
// convention.
func TestCoChangeDocumentNodeEndpoint(t *testing.T) {
	commits := []gitlog.Commit{
		{Files: []string{"specs/x.md", "internal/x/x.go"}},
		{Files: []string{"specs/x.md", "internal/x/x.go"}},
	}
	nodes := []Node{
		{ID: "docX", Kind: NodeDocument, FilePath: "specs/x.md"},
		fileNode("fileX", "internal/x/x.go"),
	}

	edges := coChangeEdgesFromCommits(commits, nodes)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	e := edges[0]
	if (e.SrcID != "docX" || e.DstID != "fileX") && (e.SrcID != "fileX" || e.DstID != "docX") {
		t.Errorf("SrcID/DstID = %s/%s, want docX/fileX in some order", e.SrcID, e.DstID)
	}
}

// TestCoChangeMaxFilesPerCommitGuard checks a commit touching more than
// maxFilesPerCommit files contributes zero pairs and zero commitsTouching.
func TestCoChangeMaxFilesPerCommitGuard(t *testing.T) {
	var noisy []string
	for i := 0; i < coChangeMaxFilesPerCommit+1; i++ {
		noisy = append(noisy, "f"+string(rune('a'+i%26))+".go")
	}
	commits := []gitlog.Commit{
		{Files: noisy},
		// A normal commit that would otherwise establish support with a file
		// from the noisy commit — if the guard leaked counts, this pair
		// would need only 1 more co-occurrence to hit support=2.
		{Files: []string{noisy[0], noisy[1]}},
	}
	var nodes []Node
	for i, f := range noisy {
		nodes = append(nodes, fileNode("id"+string(rune('a'+i)), f))
	}

	edges := coChangeEdgesFromCommits(commits, nodes)
	if len(edges) != 0 {
		t.Fatalf("got %d edges, want 0 (noisy commit must not contribute any counts): %+v", len(edges), edges)
	}
}

// TestCoChangeFileExistenceFilter checks a pair is dropped when either file
// has no corresponding NodeFile node in the current graph.
func TestCoChangeFileExistenceFilter(t *testing.T) {
	commits := []gitlog.Commit{
		{Files: []string{"a.go", "gone.go"}},
		{Files: []string{"a.go", "gone.go"}},
	}
	// Only a.go has a file node — gone.go was deleted/never extracted.
	nodes := []Node{fileNode("fileA", "a.go")}

	edges := coChangeEdgesFromCommits(commits, nodes)
	if len(edges) != 0 {
		t.Fatalf("got %d edges, want 0 (gone.go has no file node): %+v", len(edges), edges)
	}
}

// TestCoChangeDedupOrdering checks the same pair, whatever order it appears
// in per-commit file lists, yields exactly one edge with SrcID < DstID.
func TestCoChangeDedupOrdering(t *testing.T) {
	commits := []gitlog.Commit{
		{Files: []string{"b.go", "a.go"}}, // b before a in this commit's list
		{Files: []string{"a.go", "b.go"}}, // a before b in this one
	}
	nodes := []Node{fileNode("fileB", "b.go"), fileNode("fileA", "a.go")}

	edges := coChangeEdgesFromCommits(commits, nodes)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1 (deduped): %+v", len(edges), edges)
	}
	if edges[0].SrcID >= edges[0].DstID {
		t.Errorf("SrcID/DstID = %s/%s, want SrcID < DstID", edges[0].SrcID, edges[0].DstID)
	}
	if edges[0].SrcID != "fileA" || edges[0].DstID != "fileB" {
		t.Errorf("SrcID/DstID = %s/%s, want fileA/fileB", edges[0].SrcID, edges[0].DstID)
	}
	support, _ := edges[0].Metadata["support"].(float64)
	if support != 2 {
		t.Errorf("support = %v, want 2 (both orderings count toward the same pair)", edges[0].Metadata["support"])
	}
}

// TestCoChangeEdgesDeterministicOrder checks the returned edge slice is
// sorted (reproducible across runs, independent of map iteration order).
func TestCoChangeEdgesDeterministicOrder(t *testing.T) {
	commits := []gitlog.Commit{
		{Files: []string{"a.go", "b.go", "c.go"}},
		{Files: []string{"a.go", "b.go", "c.go"}},
	}
	nodes := []Node{fileNode("fileA", "a.go"), fileNode("fileB", "b.go"), fileNode("fileC", "c.go")}

	edges := coChangeEdgesFromCommits(commits, nodes)
	if len(edges) != 3 {
		t.Fatalf("got %d edges, want 3 (a-b, a-c, b-c)", len(edges))
	}
	if !sort.SliceIsSorted(edges, func(i, j int) bool {
		if edges[i].SrcID != edges[j].SrcID {
			return edges[i].SrcID < edges[j].SrcID
		}
		return edges[i].DstID < edges[j].DstID
	}) {
		t.Errorf("edges not sorted: %+v", edges)
	}
}
