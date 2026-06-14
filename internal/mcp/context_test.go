package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/store"
)

// ─── pickSuggestedReads ───────────────────────────────────────────────────

func TestPickSuggestedReadsSymbolIntent(t *testing.T) {
	syms := []SymbolHit{
		{QualifiedName: "Foo", Path: "a.go", StartLine: 10, EndLine: 20},
		{QualifiedName: "Bar", Path: "b.go", StartLine: 5, EndLine: 15},
	}
	got := pickSuggestedReads(retrieve.IntentSymbolLookup, nil, syms, nil, nil)
	if len(got) != 2 || got[0].Path != "a.go" || got[1].Path != "b.go" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Reason != "definition of Foo" {
		t.Errorf("reason=%q", got[0].Reason)
	}
}

func TestPickSuggestedReadsCrossLaneBias(t *testing.T) {
	// b.go appears in both lanes; a.go is semantic-only with higher score.
	// Cross-lane agreement should bump b.go to the top regardless of score.
	sem := []SemHit{
		{Path: "a.go", StartLine: 1, EndLine: 10, Score: 0.9},
		{Path: "b.go", StartLine: 1, EndLine: 10, Score: 0.7},
	}
	symbolPaths := map[string]struct{}{"b.go": {}}

	got := pickSuggestedReads(retrieve.IntentBehaviorSearch, sem, nil, symbolPaths, nil)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Path != "b.go" {
		t.Errorf("cross-lane should win: got %s first, want b.go", got[0].Path)
	}
	if !strings.Contains(got[0].Reason, "symbol agreement") {
		t.Errorf("reason should mention symbol agreement: %q", got[0].Reason)
	}
}

func TestPickSuggestedReadsDocScoreWinsForBehavior(t *testing.T) {
	// For behavior_search, a spec/behavior doc scoring higher should
	// beat a code file — e.g. specs/watch.md is the right answer for
	// "how does watch work?". No code-preference tiebreaker applies.
	sem := []SemHit{
		{Path: "README.md", Score: 0.66},
		{Path: "internal/store/store.go", Score: 0.51},
	}
	got := pickSuggestedReads(retrieve.IntentBehaviorSearch, sem, nil, nil, nil)
	if len(got) == 0 || got[0].Path != "README.md" {
		t.Errorf("behavior_search: higher-scoring doc should win; got %+v", got)
	}

	// Architecture — same: README wins by score.
	gotArch := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, nil)
	if len(gotArch) == 0 || gotArch[0].Path != "README.md" {
		t.Errorf("architecture should keep README on top; got %+v", gotArch)
	}

	// Other code-oriented intents still apply the tiebreaker.
	gotEdit := pickSuggestedReads(retrieve.IntentEditingContext, sem, nil, nil, nil)
	if len(gotEdit) == 0 || gotEdit[0].Path != "internal/store/store.go" {
		t.Errorf("editing_context should prefer .go over README tiebreaker; got %+v", gotEdit)
	}
}

func TestPickSuggestedReadsCodePreferredOverBuildFiles(t *testing.T) {
	// Taskfile.yml shouldn't beat the .go file it wraps when the
	// intent is implementation-oriented. Architecture intentionally
	// keeps the build file since it can reveal structure.
	sem := []SemHit{
		{Path: "Taskfile.yml", Score: 0.66},
		{Path: "internal/mcp/server.go", Score: 0.40},
	}
	got := pickSuggestedReads(retrieve.IntentEditingContext, sem, nil, nil, nil)
	if len(got) == 0 || got[0].Path != "internal/mcp/server.go" {
		t.Errorf("editing_context should prefer .go over Taskfile.yml; got %+v", got)
	}
	gotArch := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, nil)
	if len(gotArch) == 0 || gotArch[0].Path != "Taskfile.yml" {
		t.Errorf("architecture should leave score order intact; got %+v", gotArch)
	}
}

// TestPickSuggestedReadsPageRankTiebreaker verifies that for
// architecture / package_topology intents, two hits whose scores fall
// in the same 0.05-wide bucket are reordered by PageRank — so a
// structural hub like `Indexer.Run` beats a near-tied non-hub.
// Outside that bucket, score continues to dominate. Non-exploration
// intents keep the strict score order untouched.
func TestPickSuggestedReadsPageRankTiebreaker(t *testing.T) {
	// view stub: hub.go has a node with PageRank 0.8, ordinary.go is
	// 0.05. Both files have one whole-file-spanning function node so
	// chunkPageRank's "covering" path resolves.
	view := &graphquery.View{
		NodesByPath: map[string][]graphquery.Node{
			"hub.go":      {{ID: "hub", Kind: graph.NodeFunction, FilePath: "hub.go", StartLine: 1, EndLine: 100, PageRank: 0.8}},
			"ordinary.go": {{ID: "ord", Kind: graph.NodeFunction, FilePath: "ordinary.go", StartLine: 1, EndLine: 100, PageRank: 0.05}},
		},
	}

	t.Run("hub wins inside same score bucket for architecture", func(t *testing.T) {
		// 0.55 and 0.58 both land in bucket 11 (int(score*20) == 11).
		// Without centrality the higher-scored ordinary.go would win.
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, view)
		if len(got) == 0 || got[0].Path != "hub.go" {
			t.Errorf("PageRank should flip ordering within score bucket; got %+v", got)
		}
	})

	t.Run("score still dominates across buckets for architecture", func(t *testing.T) {
		// 0.70 vs 0.50 — different buckets (14 vs 10). Hub's PageRank
		// can't flip this: the score gap is real, not marginal.
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.70},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.50},
		}
		got := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, view)
		if len(got) == 0 || got[0].Path != "ordinary.go" {
			t.Errorf("score-bucket gap should keep ordinary.go on top; got %+v", got)
		}
	})

	t.Run("package_topology also uses centrality", func(t *testing.T) {
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := pickSuggestedReads(retrieve.IntentPackageTopology, sem, nil, nil, view)
		if len(got) == 0 || got[0].Path != "hub.go" {
			t.Errorf("package_topology should also reorder by PageRank; got %+v", got)
		}
	})

	t.Run("behavior_search ignores PageRank — strict score order", func(t *testing.T) {
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := pickSuggestedReads(retrieve.IntentBehaviorSearch, sem, nil, nil, view)
		if len(got) == 0 || got[0].Path != "ordinary.go" {
			t.Errorf("behavior_search should keep score order regardless of PageRank; got %+v", got)
		}
	})

	t.Run("nil view degrades to pure score order for architecture", func(t *testing.T) {
		// No graph loaded — chunkPageRank returns 0 for everyone, so
		// the bucket tiebreaker is a wash and the comparator falls
		// through to the final score-compare line.
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, nil)
		if len(got) == 0 || got[0].Path != "ordinary.go" {
			t.Errorf("nil view should fall back to score order; got %+v", got)
		}
	})
}

func TestIsBuildOrConfigPath(t *testing.T) {
	tests := map[string]bool{
		"Taskfile.yml":   true,
		"Taskfile.yaml":  true,
		"Dockerfile":     true,
		"Makefile":       true,
		".github/ci.yml": true,
		"config.toml":    true,
		"internal/x.go":  false,
		"README.md":      false,
		"go.mod":         false, // intentionally not demoted
		"package.json":   false,
	}
	for p, want := range tests {
		if got := pathTags(p).has(tagBuild); got != want {
			t.Errorf("pathTags(%q).has(tagBuild) = %v, want %v", p, got, want)
		}
	}
}

func TestIsDocPath(t *testing.T) {
	tests := map[string]bool{
		"README.md":               true,
		"docs/spec.rst":           true,
		"NOTES.txt":               true,
		"docs/page.adoc":          true,
		"site/post.mdx":           true,
		"internal/store/store.go": false,
		"cmd/main.py":             false,
	}
	for p, want := range tests {
		if got := pathTags(p).has(tagDoc); got != want {
			t.Errorf("pathTags(%q).has(tagDoc) = %v, want %v", p, got, want)
		}
	}
}

func TestIsTestPath(t *testing.T) {
	tests := map[string]bool{
		"internal/mcp/context_test.go": true,
		"pkg/foo/bar_test.go":          true,
		"src/Foo.test.ts":              true,
		"src/Foo.test.tsx":             true,
		"src/foo.spec.js":              true,
		"src/foo.spec.jsx":             true,
		"tests/test_foo.py":            true,
		"tests/foo_test.py":            true,
		"spec/foo_spec.rb":             true,
		"src/foo_test.rs":              true,
		"internal/store/store.go":      false,
		"README.md":                    false,
		"cmd/main.py":                  false,
		"src/foo.ts":                   false,
	}
	for p, want := range tests {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// ─── enrichGraph caps ─────────────────────────────────────────────────────

// TestEnrichGraphCaps guards against unbounded graph payloads — the
// regression that motivated maxGraphNodes/maxGraphEdges. A big
// package's rollup or a god-struct's sibling fan-out should not blow
// the response budget.
func TestEnrichGraphCaps(t *testing.T) {
	t.Run("node cap via package rollup", func(t *testing.T) {
		view := &graphquery.View{
			NodesByID:        map[string]graphquery.Node{},
			NodesByName:      map[string][]graphquery.Node{},
			NodesByQualified: map[string][]graphquery.Node{},
			NodesByPackage:   map[string][]graphquery.Node{},
			NodesByPath:      map[string][]graphquery.Node{},
			EdgesBySrc:       map[string][]graphquery.Edge{},
			EdgesByDst:       map[string][]graphquery.Edge{},
			EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
		}
		const pkg = "example.com/bigpkg"
		for i := range 100 {
			n := graphquery.Node{
				ID:            fmt.Sprintf("n%d", i),
				Kind:          graph.NodeFunction,
				Name:          fmt.Sprintf("Fn%d", i),
				QualifiedName: fmt.Sprintf("Fn%d", i),
				PackagePath:   pkg,
				FilePath:      "bigpkg/bigpkg.go",
			}
			view.NodesByID[n.ID] = n
			view.NodesByPackage[pkg] = append(view.NodesByPackage[pkg], n)
			view.NodesByPath[n.FilePath] = append(view.NodesByPath[n.FilePath], n)
		}
		out := &ContextOutput{}
		enrichGraph(out, retrieve.IntentArchitecture, view, []SemHit{{Path: "bigpkg/bigpkg.go"}}, nil)
		if got := len(out.Graph.Nodes); got > retrieve.MaxGraphNodes {
			t.Errorf("got %d nodes, want ≤ %d", got, retrieve.MaxGraphNodes)
		}
		if len(out.Graph.Nodes) == 0 {
			t.Error("expected some nodes from package rollup")
		}
	})

	t.Run("edge cap via package_topology imports", func(t *testing.T) {
		view := &graphquery.View{
			NodesByID:        map[string]graphquery.Node{},
			NodesByName:      map[string][]graphquery.Node{},
			NodesByQualified: map[string][]graphquery.Node{},
			NodesByPackage:   map[string][]graphquery.Node{},
			NodesByPath:      map[string][]graphquery.Node{},
			EdgesBySrc:       map[string][]graphquery.Edge{},
			EdgesByDst:       map[string][]graphquery.Edge{},
			EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
		}
		const src = "example.com/src"
		srcPkg := graphquery.Node{ID: "src", Kind: graph.NodePackage, Name: "src", PackagePath: src, FilePath: "src/src.go"}
		view.NodesByID[srcPkg.ID] = srcPkg
		view.NodesByPackage[src] = append(view.NodesByPackage[src], srcPkg)
		view.NodesByPath[srcPkg.FilePath] = append(view.NodesByPath[srcPkg.FilePath], srcPkg)
		for i := range 100 {
			dstID := fmt.Sprintf("dst%d", i)
			dst := graphquery.Node{ID: dstID, Kind: graph.NodePackage, Name: dstID, PackagePath: "example.com/" + dstID}
			view.NodesByID[dstID] = dst
			e := graphquery.Edge{Kind: graph.EdgeImports, SrcID: srcPkg.ID, DstID: dstID}
			view.EdgesByKind[graph.EdgeImports] = append(view.EdgesByKind[graph.EdgeImports], e)
			view.EdgesBySrc[srcPkg.ID] = append(view.EdgesBySrc[srcPkg.ID], e)
			view.EdgesByDst[dstID] = append(view.EdgesByDst[dstID], e)
		}
		out := &ContextOutput{}
		enrichGraph(out, retrieve.IntentPackageTopology, view, []SemHit{{Path: "src/src.go"}}, nil)
		if got := len(out.Graph.Edges); got > retrieve.MaxGraphEdges {
			t.Errorf("got %d edges, want ≤ %d", got, retrieve.MaxGraphEdges)
		}
		if len(out.Graph.Edges) == 0 {
			t.Error("expected some edges from imports rollup")
		}
	})
}

// TestArchitectureAnchorsOnPageRank guards the fix for the degenerate
// case where a docs-dominated semantic lane collapses the architecture
// rollup to whichever single Go file leaked through. With PageRank
// anchoring the rollup must surface the project's central packages
// even when semHits point only at non-Go paths.
func TestArchitectureAnchorsOnPageRank(t *testing.T) {
	view := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPackage:   map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	// Three packages, descending centrality: core (hub), mid, tangent.
	type pkgSpec struct {
		path string
		file string
		pr   float64
	}
	specs := []pkgSpec{
		{"example.com/core", "core/core.go", 0.9},
		{"example.com/mid", "mid/mid.go", 0.4},
		{"example.com/tangent", "tangent/tangent.go", 0.05},
	}
	for _, s := range specs {
		pkgNode := graphquery.Node{
			ID: s.path + "::pkg", Kind: graph.NodePackage,
			Name: retrieve.PkgTail(s.path), PackagePath: s.path,
			FilePath: s.file, PageRank: s.pr,
		}
		view.NodesByID[pkgNode.ID] = pkgNode
		view.NodesByPackage[s.path] = append(view.NodesByPackage[s.path], pkgNode)
		view.NodesByPath[s.file] = append(view.NodesByPath[s.file], pkgNode)
	}
	// semHits point only at a doc file — no graph nodes there.
	out := &ContextOutput{}
	enrichGraph(out, retrieve.IntentArchitecture, view, []SemHit{{Path: "README.md"}}, nil)
	if out.Graph == nil {
		t.Fatal("expected graph rollup anchored on PageRank when semHits are docs")
	}
	got := map[string]bool{}
	for _, n := range out.Graph.Nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"core", "mid", "tangent"} {
		if !got[want] {
			t.Errorf("expected pkg %q in rollup; got nodes %v", want, out.Graph.Nodes)
		}
	}
}

// TestArchitectureAnchorAugmentedBySemHits verifies that the PageRank
// anchor union still pulls in subsystem-specific packages when the user
// names one. The architecture rollup should be hub ∪ requested.
func TestArchitectureAnchorAugmentedBySemHits(t *testing.T) {
	view := &graphquery.View{
		NodesByID:        map[string]graphquery.Node{},
		NodesByName:      map[string][]graphquery.Node{},
		NodesByQualified: map[string][]graphquery.Node{},
		NodesByPackage:   map[string][]graphquery.Node{},
		NodesByPath:      map[string][]graphquery.Node{},
		EdgesBySrc:       map[string][]graphquery.Edge{},
		EdgesByDst:       map[string][]graphquery.Edge{},
		EdgesByKind:      map[graph.EdgeKind][]graphquery.Edge{},
	}
	hub := graphquery.Node{
		ID: "hub::pkg", Kind: graph.NodePackage,
		Name: "hub", PackagePath: "example.com/hub",
		FilePath: "hub/hub.go", PageRank: 0.9,
	}
	leaf := graphquery.Node{
		ID: "leaf::pkg", Kind: graph.NodePackage,
		Name: "leaf", PackagePath: "example.com/leaf",
		FilePath: "leaf/leaf.go", PageRank: 0, // no centrality
	}
	for _, n := range []graphquery.Node{hub, leaf} {
		view.NodesByID[n.ID] = n
		view.NodesByPackage[n.PackagePath] = append(view.NodesByPackage[n.PackagePath], n)
		view.NodesByPath[n.FilePath] = append(view.NodesByPath[n.FilePath], n)
	}
	out := &ContextOutput{}
	enrichGraph(out, retrieve.IntentArchitecture, view, []SemHit{{Path: "leaf/leaf.go"}}, nil)
	if out.Graph == nil {
		t.Fatal("expected graph rollup")
	}
	got := map[string]bool{}
	for _, n := range out.Graph.Nodes {
		got[n.ID] = true
	}
	if !got["hub"] {
		t.Error("expected hub package via PageRank anchor")
	}
	if !got["leaf"] {
		t.Error("expected leaf package via semHit augmentation")
	}
}

// ─── inlineSuggestedReads ─────────────────────────────────────────────────

func TestInlineSuggestedReadsBasic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "f.go"),
		"line 1\nline 2\nline 3\nline 4\nline 5\n")

	reads := []SuggestedRead{{Path: "f.go", StartLine: 2, EndLine: 4, Reason: "x"}}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)

	want := "line 2\nline 3\nline 4\n"
	if reads[0].Content != want {
		t.Errorf("content=%q want %q", reads[0].Content, want)
	}
	if reads[0].Truncated {
		t.Error("should not be truncated")
	}
}

func TestInlineSuggestedReadsPerReadLineCap(t *testing.T) {
	// Generate a 200-line file and ask for the whole thing. The
	// per-read cap (60 lines) should clip the content and flag it.
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeFile(t, filepath.Join(root, "big.go"), b.String())

	reads := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)

	if !reads[0].Truncated {
		t.Error("want truncated=true when range exceeds per-read cap")
	}
	got := strings.Count(reads[0].Content, "\n")
	if got > 60 {
		t.Errorf("got %d lines, want ≤60", got)
	}
	// EndLine on the wire stays as the original request so the
	// caller can issue a follow-up Read for the rest.
	if reads[0].EndLine != 200 {
		t.Errorf("EndLine=%d, want 200 (unchanged)", reads[0].EndLine)
	}
}

func TestInlineSuggestedReadsTotalByteBudget(t *testing.T) {
	// Six reads each at the per-read byte cap (4 KB) should exhaust the
	// 20 KB targeted-intent total budget before all are filled.
	root := t.TempDir()
	// Write a file with ~30 long lines so any 60-line slice hits the
	// per-read byte cap (4 KB) first.
	var b strings.Builder
	for range 30 {
		b.WriteString(strings.Repeat("x", 500))
		b.WriteByte('\n')
	}
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		writeFile(t, filepath.Join(root, n), b.String())
	}
	reads := []SuggestedRead{
		{Path: "a.go", StartLine: 1, EndLine: 30},
		{Path: "b.go", StartLine: 1, EndLine: 30},
		{Path: "c.go", StartLine: 1, EndLine: 30},
		{Path: "d.go", StartLine: 1, EndLine: 30},
		{Path: "e.go", StartLine: 1, EndLine: 30},
		{Path: "f.go", StartLine: 1, EndLine: 30},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)

	total := 0
	for _, r := range reads {
		total += len(r.Content)
	}
	if total > 20*1024 {
		t.Errorf("total inlined bytes %d > 20 KB cap", total)
	}
	// Last read should be empty — budget exhausted.
	if reads[len(reads)-1].Content != "" {
		t.Errorf("last read should be empty once budget is exhausted; got %d bytes", len(reads[len(reads)-1].Content))
	}
}

func TestInlineContentSemanticHitsAlsoFilled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "b.go"), "line A\nline B\nline C\n")

	reads := []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 3}}
	sem := []SemHit{
		{Path: "a.go", StartLine: 1, EndLine: 3},
		{Path: "b.go", StartLine: 1, EndLine: 3},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, sem)

	if sem[0].Content == "" {
		t.Error("semantic_hits[0] should be filled (cache-hit from suggested_reads)")
	}
	if sem[1].Content == "" {
		t.Error("semantic_hits[1] should be filled (separate file, within budget)")
	}
	if reads[0].Content == "" {
		t.Error("suggested_reads[0] should still be filled")
	}
}

func TestInlineContentSharedBudgetDoesNotDoubleCharge(t *testing.T) {
	// Same range appears in both lanes; the read cache should serve
	// the second request without re-charging the budget, so plenty
	// of headroom remains for other hits.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "shared.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "other.go"), "x\ny\nz\n")

	reads := []SuggestedRead{{Path: "shared.go", StartLine: 1, EndLine: 3}}
	sem := []SemHit{
		{Path: "shared.go", StartLine: 1, EndLine: 3},
		{Path: "other.go", StartLine: 1, EndLine: 3},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, sem)

	if reads[0].Content == "" || sem[0].Content == "" || sem[1].Content == "" {
		t.Errorf("expected all three to be filled; got reads=%q sem0=%q sem1=%q",
			reads[0].Content, sem[0].Content, sem[1].Content)
	}
	if reads[0].Content != sem[0].Content {
		t.Errorf("dedup cache should return identical content")
	}
}

func TestInlineContentScoreFloorOnLowSignalQueries(t *testing.T) {
	// When top semantic score is below lowConfidenceScore, hits whose
	// individual score is below noiseFloorScore should ship without
	// Content (path+range only) — the agent keeps the pointer but we
	// don't burn bytes on what's almost certainly noise.
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "weak.go"), "line 1\nline 2\nline 3\n")
	writeFile(t, filepath.Join(root, "weaker.go"), "line A\nline B\nline C\n")
	writeFile(t, filepath.Join(root, "strong.go"), "line X\nline Y\nline Z\n")

	// Top score below lowConfidenceScore (0.45) triggers the floor.
	sem := []SemHit{
		{Path: "weak.go", StartLine: 1, EndLine: 3, Score: 0.42},   // top, < 0.45 → suppression mode on
		{Path: "weaker.go", StartLine: 1, EndLine: 3, Score: 0.38}, // < noiseFloorScore → suppressed
		// 0.41 is above the floor — would also be inlined.
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, nil, nil, sem)
	if sem[0].Content == "" {
		t.Error("top hit (score 0.42) should still inline — only sub-floor hits are suppressed")
	}
	if sem[1].Content != "" {
		t.Errorf("hit at score 0.38 should be suppressed under floor; got Content=%q", sem[1].Content)
	}

	// Sanity check: when top score IS strong, low-score companions
	// still inline normally (the floor only fires on no-signal pools).
	sem2 := []SemHit{
		{Path: "strong.go", StartLine: 1, EndLine: 3, Score: 0.80},
		{Path: "weaker.go", StartLine: 1, EndLine: 3, Score: 0.38},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, nil, nil, sem2)
	if sem2[1].Content == "" {
		t.Error("low-score companion to a strong top should still inline (floor only fires on no-signal queries)")
	}
}

func TestInlineSuggestedReadsMissingFileGraceful(t *testing.T) {
	root := t.TempDir()
	reads := []SuggestedRead{{Path: "does-not-exist.go", StartLine: 1, EndLine: 5}}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil) // must not panic
	if reads[0].Content != "" {
		t.Errorf("missing file should leave content empty, got %q", reads[0].Content)
	}
}

func TestInlineContentFillsSymbolBodyForSymbolLookup(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"),
		"package main\n"+
			"\n"+
			"func Alpha() string {\n"+
			"\treturn \"alpha\"\n"+
			"}\n")
	syms := []SymbolHit{
		{QualifiedName: "Alpha", Path: "a.go", StartLine: 3, EndLine: 5},
	}
	inlineContent(root, retrieve.IntentSymbolLookup, nil, syms, nil)
	if syms[0].Body == "" {
		t.Fatal("symbol body should be inlined for symbol_lookup intent")
	}
	if !strings.Contains(syms[0].Body, "return \"alpha\"") {
		t.Errorf("symbol body should include the function body; got %q", syms[0].Body)
	}
}

func TestInlineContentFillsImportsForGoFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "deep/foo.go"),
		"package deep\n"+
			"\n"+
			"import (\n"+
			"\t\"context\"\n"+
			"\t\"fmt\"\n"+
			"\t\"github.com/x/y\"\n"+
			")\n"+
			"\n"+
			"func A() {}\n"+
			"func B() {}\n"+
			"func C() {}\n"+
			"func D() {}\n"+
			"func E() {}\n"+
			"func F() {}\n"+
			"func G() {}\n"+
			"func H() {}\n"+
			"func I() {}\n"+
			"func J() {}\n"+
			"func K() {}\n"+
			"func L() {}\n"+
			"func M() {}\n"+
			"func N() {}\n")
	reads := []SuggestedRead{
		// Reads a function far from the import block (StartLine > 5).
		{Path: "deep/foo.go", StartLine: 20, EndLine: 22, Reason: "test"},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)
	if reads[0].Imports == "" {
		t.Fatal("Go imports should be inlined for a read starting away from the top")
	}
	for _, want := range []string{"import (", "\"context\"", "\"fmt\"", "\"github.com/x/y\"", ")"} {
		if !strings.Contains(reads[0].Imports, want) {
			t.Errorf("imports missing %q; got:\n%s", want, reads[0].Imports)
		}
	}
}

func TestInlineContentSkipsImportsWhenReadCoversTop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "foo.go"),
		"package foo\n\nimport \"context\"\n\nfunc A() {}\n")
	reads := []SuggestedRead{
		// StartLine=1 → the read already includes the import line.
		{Path: "foo.go", StartLine: 1, EndLine: 5, Reason: "test"},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)
	if reads[0].Imports != "" {
		t.Errorf("Imports should be omitted when the read already covers the top; got %q", reads[0].Imports)
	}
}

func TestInlineContentSkipsImportsForUnknownLanguage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "foo.txt"), "plain text\nno imports here\n")
	reads := []SuggestedRead{
		{Path: "foo.txt", StartLine: 20, EndLine: 22, Reason: "test"},
	}
	inlineContent(root, retrieve.IntentBehaviorSearch, reads, nil, nil)
	if reads[0].Imports != "" {
		t.Errorf("unknown language should produce empty Imports; got %q", reads[0].Imports)
	}
}

func TestInlineContentSkipsSymbolBodyForOtherIntents(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.go"),
		"package main\n\nfunc Alpha() {}\n")
	syms := []SymbolHit{
		{QualifiedName: "Alpha", Path: "a.go", StartLine: 3, EndLine: 3},
	}
	// behavior_search should NOT fill bodies — signature+doc are
	// considered sufficient and we save budget for semantic_hits.
	inlineContent(root, retrieve.IntentBehaviorSearch, nil, syms, nil)
	if syms[0].Body != "" {
		t.Errorf("non-symbol_lookup intent should leave Body empty; got %q", syms[0].Body)
	}
}

func TestContextRouterInlinesByDefault(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("want suggested_reads")
	}
	if out.SuggestedReads[0].Content == "" {
		t.Errorf("suggested_reads[0].Content should be inlined by default; got empty")
	}
}

func TestContextRouterNoInline(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
		NoInline:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("want suggested_reads")
	}
	for i, r := range out.SuggestedReads {
		if r.Content != "" {
			t.Errorf("suggested_reads[%d].Content should be empty with NoInline=true; got %d bytes", i, len(r.Content))
		}
	}
}

// TestInlineContentSkipsTestSourceForNonEditing verifies that raw test
// source in semantic_hits arrives with Path/Range only (no Content) for
// non-editing intents, while the matching implementation chunk is
// inlined. Test bodies displaced implementation from the shared inline
// budget before this filter — surfaced when "architecture" / behavior
// queries pulled foo_test.go above foo.go and burned ~4 KB on fixture
// boilerplate. editing_context is the exception: sibling tests are
// real context when modifying a file.
//
// Unit-tests inlineContent directly with pre-populated hits so the
// suppressLowScore branch (fake-embed cosines are too low to clear the
// confidence threshold) doesn't mask the filter's behavior.
func TestInlineContentSkipsTestSourceForNonEditing(t *testing.T) {
	projDir := t.TempDir()
	implPath := filepath.Join(projDir, "greet.go")
	testPath := filepath.Join(projDir, "greet_test.go")
	writeFile(t, implPath,
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	writeFile(t, testPath,
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"x\") == \"\" {\n\t\tt.Fatal(\"oops\")\n\t}\n}\n")

	mkHits := func() []SemHit {
		// Scores set above lowConfidenceScore so suppressLowScore stays
		// off — isolates the test-path filter as the only suppression.
		return []SemHit{
			{Path: "greet.go", StartLine: 3, EndLine: 3, Score: 0.9, Kind: "function_declaration"},
			{Path: "greet_test.go", StartLine: 5, EndLine: 9, Score: 0.8, Kind: "function_declaration"},
		}
	}

	t.Run("behavior_search drops test content", func(t *testing.T) {
		sem := mkHits()
		inlineContent(projDir, retrieve.IntentBehaviorSearch, nil, nil, sem)
		if sem[0].Content == "" {
			t.Errorf("impl greet.go should be inlined; got empty")
		}
		if sem[1].Content != "" {
			t.Errorf("test greet_test.go should be path-only; got %d bytes", len(sem[1].Content))
		}
	})

	t.Run("architecture drops test content", func(t *testing.T) {
		sem := mkHits()
		inlineContent(projDir, retrieve.IntentArchitecture, nil, nil, sem)
		if sem[1].Content != "" {
			t.Errorf("architecture: test should be path-only; got %d bytes", len(sem[1].Content))
		}
	})

	t.Run("editing_context keeps test content", func(t *testing.T) {
		sem := mkHits()
		inlineContent(projDir, retrieve.IntentEditingContext, nil, nil, sem)
		if sem[1].Content == "" {
			t.Errorf("editing_context: sibling test should be inlined; got empty")
		}
	})
}

func TestMaxSemanticScore(t *testing.T) {
	// Observed in production: the router was reading score from
	// hits[0], but semantic_hits is reordered by mergeSummaryHits and
	// rerank — so a low-score symbol-driven entry at the head was
	// fooling buildNextAction into emitting "weak match" even when a
	// strong hit existed further down.
	hits := []SemHit{
		{Path: "noise.go", Score: 0.01},  // promoted to front by re-ordering
		{Path: "strong.go", Score: 0.85}, // the real best match
		{Path: "mid.go", Score: 0.42},
	}
	if got := maxSemanticScore(hits); got != 0.85 {
		t.Errorf("maxSemanticScore = %v, want 0.85 (must scan all hits, not just [0])", got)
	}

	// Empty input → 0 (no confidence claim).
	if got := maxSemanticScore(nil); got != 0 {
		t.Errorf("empty hits should yield 0; got %v", got)
	}
}

func TestIsFixturePath(t *testing.T) {
	cases := map[string]bool{
		"internal/graph/testdata/simple/store/store.go": true,
		"pkg/testdata/foo.go":                           true,
		"web/__fixtures__/users.json":                   true,
		"internal/store/store.go":                       false,
		"internal/test_helpers.go":                      false, // not in a testdata dir
		"docs/README.md":                                false,
	}
	for path, want := range cases {
		if got := pathTags(path).has(tagFixture); got != want {
			t.Errorf("pathTags(%q).has(tagFixture) = %v, want %v", path, got, want)
		}
	}
}

func TestRunSymbolLaneDemotesFixtures(t *testing.T) {
	// FindSymbol orders by (path, start_line), so `internal/graph/
	// testdata/simple/store/store.go` lands BEFORE `internal/store/
	// store.go` alphabetically. The symbol lane must demote testdata
	// paths so the prose directive points at real code.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "real.go"),
		"package main\n\ntype Store struct{}\n")
	writeFile(t, filepath.Join(projDir, "testdata", "fixture.go"),
		"package fixture\n\ntype Store struct{}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Store",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Symbols) < 2 {
		t.Fatalf("expected 2 symbols (real + testdata); got %d", len(out.Symbols))
	}
	if out.Symbols[0].Path != "real.go" {
		t.Errorf("first symbol should be real.go (not testdata); got %q", out.Symbols[0].Path)
	}
	if !strings.Contains(out.Symbols[1].Path, "testdata") {
		t.Errorf("second symbol should be the testdata fixture; got %q", out.Symbols[1].Path)
	}
}

func TestPickSuggestedReadsFiltersRollupSummaries(t *testing.T) {
	// package_summary / repo_summary rollup chunks have Path pointing
	// at a directory and zero line ranges. They're informative in the
	// semantic_hits lane but produce bogus "lines 0-0" Read directives
	// if they leak into suggested_reads.
	sem := []SemHit{
		{Path: "internal/index", Kind: "package_summary", Score: 0.95},
		{Path: ".", Kind: "repo_summary", Score: 0.90},
		{Path: "internal/store/store.go", StartLine: 100, EndLine: 150, Kind: "method_declaration", Score: 0.85},
	}
	got := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, nil)
	for _, r := range got {
		if r.Path == "internal/index" || r.Path == "." {
			t.Errorf("rollup-summary path %q leaked into suggested_reads", r.Path)
		}
	}
	// At least the real file should make it through.
	foundFile := false
	for _, r := range got {
		if r.Path == "internal/store/store.go" {
			foundFile = true
		}
	}
	if !foundFile {
		t.Error("real-file SemHit should land in suggested_reads")
	}
}

func TestPickSuggestedReadsArchitectureCap(t *testing.T) {
	sem := []SemHit{
		{Path: "a.go", Score: 0.9}, {Path: "b.go", Score: 0.8},
		{Path: "c.go", Score: 0.7}, {Path: "d.go", Score: 0.6},
		{Path: "e.go", Score: 0.5}, {Path: "f.go", Score: 0.4},
	}
	got := pickSuggestedReads(retrieve.IntentArchitecture, sem, nil, nil, nil)
	// Exploration intents widen to 5 reads so the initial bundle gives
	// the caller a real cross-file picture.
	if len(got) != 5 {
		t.Errorf("architecture should return 5 reads, got %d", len(got))
	}
}

// TestInlineCapsFor moved to internal/retrieve (inline_test.go) with the
// caps policy itself — the budget is transport-free.

func TestInlineSuggestedReadsExplorationDenser(t *testing.T) {
	// 200-line file requested in full. targeted caps clip at 60 lines;
	// exploration caps should clip at 120.
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeFile(t, filepath.Join(root, "big.go"), b.String())

	targeted := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	inlineContent(root, retrieve.IntentBehaviorSearch, targeted, nil, nil)
	targetedLines := strings.Count(targeted[0].Content, "\n")

	exploration := []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 200}}
	inlineContent(root, retrieve.IntentArchitecture, exploration, nil, nil)
	explorationLines := strings.Count(exploration[0].Content, "\n")

	if !(explorationLines > targetedLines) {
		t.Errorf("exploration should include more lines than targeted; got %d vs %d", explorationLines, targetedLines)
	}
	if explorationLines > 120 {
		t.Errorf("exploration line count %d exceeds expected 120 cap", explorationLines)
	}
}

// ─── buildNextAction / buildAvoid (prose) ─────────────────────────────────

func TestBuildNextAction(t *testing.T) {
	reads := []SuggestedRead{{Path: "x.go", StartLine: 10, EndLine: 30}}
	syms := []SymbolHit{{QualifiedName: "Foo", Path: "x.go"}}

	cases := []struct {
		intent     string
		reads      []SuggestedRead
		syms       []SymbolHit
		topSem     float32
		graphEdges int
		refs       int
		hasBlame   bool
		want       string // substring match
	}{
		{retrieve.IntentSymbolLookup, reads, syms, 0.8, 0, 0, false, "Read x.go lines 10-30"},
		// symbol_lookup ambiguous: 3 symbols across 3 distinct paths —
		// next_action must signal the count, not say "the definition".
		{retrieve.IntentSymbolLookup, reads, []SymbolHit{
			{QualifiedName: "Options", Path: "a.go"},
			{QualifiedName: "Options", Path: "b.go"},
			{QualifiedName: "Options", Path: "c.go"},
		}, 0.8, 0, 0, false, "3 definitions across files"},
		// symbol_lookup with NO symbols but a strong semantic hit:
		// next_action must not claim "the definition" — that lied
		// when the symbol genuinely wasn't found. Soft fallback.
		{retrieve.IntentSymbolLookup, reads, nil, 0.8, 0, 0, false, "No exact symbol match"},
		{retrieve.IntentEditingContext, reads, syms, 0.8, 0, 0, false, "before editing"},
		{retrieve.IntentBehaviorSearch, reads, syms, 0.8, 0, 0, false, "ground your answer"},
		{retrieve.IntentArchitecture, reads, syms, 0.8, 0, 0, false, "structural overview"},
		// callers with graph edges: directive points at the graph lane.
		{retrieve.IntentCallers, nil, syms, 0.8, 4, 0, false, "4 callers edges"},
		// singular fallback for "1 edge".
		{retrieve.IntentCallers, nil, syms, 0.8, 1, 0, false, "1 callers edge"},
		// callers without graph edges but with rg-backed references.
		{retrieve.IntentCallers, nil, syms, 0.8, 0, 3, false, "3 call sites"},
		{retrieve.IntentCallers, nil, syms, 0.8, 0, 1, false, "1 call site"},
		// callers with neither: soft "start from the symbol" line.
		{retrieve.IntentCallers, nil, syms, 0.8, 0, 0, false, "No callers found"},
		{retrieve.IntentSymbolLookup, nil, nil, 0, 0, 0, false, "Rephrase"},
		// Low-confidence: no symbols and top semantic score below the
		// threshold should route to the "weak match" branch instead of
		// confidently pointing at a noise hit.
		{retrieve.IntentBehaviorSearch, reads, nil, 0.30, 0, 0, false, "Top semantic match is weak"},
		// Symbols present — confidence comes from the structural lane,
		// so the low-score branch must NOT trigger.
		{retrieve.IntentBehaviorSearch, reads, syms, 0.30, 0, 0, false, "ground your answer"},
		// package_topology with graph edges populated: weak-score branch
		// must NOT fire — the graph IS the payload. Expect the graph
		// directive, not the rephrase fallback.
		{retrieve.IntentPackageTopology, reads, nil, 0.30, 47, 0, false, "graph.edges"},
		{retrieve.IntentPackageTopology, reads, nil, 0.30, 47, 0, false, "47 imports"},
		// editing_context with blame annotations: weak-score branch
		// must NOT fire — point at the primary site.
		{retrieve.IntentEditingContext, reads, nil, 0.30, 0, 0, true, "before editing"},
		// package_topology with empty graph still routes to weak-match
		// fallback when scores are low (the genuine no-signal case).
		{retrieve.IntentPackageTopology, reads, nil, 0.30, 0, 0, false, "Top semantic match is weak"},
	}
	for _, tc := range cases {
		t.Run(tc.intent+" "+tc.want, func(t *testing.T) {
			got := buildNextAction(tc.intent, tc.reads, tc.syms, tc.topSem, tc.graphEdges, tc.refs, tc.hasBlame)
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestBuildAvoid(t *testing.T) {
	sem := []SemHit{{Path: "a.go"}}
	syms := []SymbolHit{{QualifiedName: "Foo", Path: "a.go"}}

	cases := []struct {
		name         string
		intent       string
		sem          []SemHit
		syms         []SymbolHit
		graphIndexed bool
		want         string
	}{
		{"callers warns non-Go fallback", retrieve.IntentCallers, sem, syms, true, "`calls` edges are Go-only"},
		{"callers without graph still warns", retrieve.IntentCallers, sem, syms, false, "`calls` edges are Go-only"},
		{"symbol_lookup without graph nudges to index", retrieve.IntentSymbolLookup, sem, syms, false, "Run `dex index"},
		{"symbol_lookup with graph: don't grep", retrieve.IntentSymbolLookup, sem, syms, true, "Do not grep"},
		{"behavior + both lanes", retrieve.IntentBehaviorSearch, sem, syms, true, "Do not grep for the identifier"},
		{"behavior + symbols only", retrieve.IntentBehaviorSearch, nil, syms, true, "Do not grep for the identifier"},
		{"behavior + semantic only", retrieve.IntentBehaviorSearch, sem, nil, true, "Do not read entire files"},
		{"behavior + nothing", retrieve.IntentBehaviorSearch, nil, nil, true, ""},
		// behavior_search without graph now also gets the index nag —
		// graph enrichment runs on every intent.
		{"behavior without graph nudges to index", retrieve.IntentBehaviorSearch, sem, syms, false, "Run `dex index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAvoid(tc.intent, tc.sem, tc.syms, tc.graphIndexed, false)
			if tc.want == "" {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want substring %q", got, tc.want)
			}
		})
	}
}

// ─── integration: contextRouter end-to-end ────────────────────────────────

func TestContextRouterBehaviorSearch(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "where do we greet users",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Intent != retrieve.IntentBehaviorSearch {
		t.Errorf("intent=%s, want behavior_search", out.Intent)
	}
	if len(out.SemanticHits) == 0 {
		t.Fatal("want semantic_hits, got 0")
	}
	if len(out.SuggestedReads) == 0 {
		t.Error("want suggested_reads")
	}
	if out.NextAction == "" {
		t.Error("want non-empty next_action prose")
	}
	// out.Graph is omitted when enrichGraph produced nothing — the
	// JSON tag is `omitempty`. We don't assert anything about it here;
	// the dedicated TestContextRouter*GraphPopulated tests cover the
	// populated path.
}

func TestContextRouterSymbolLookup(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Greet",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Intent != retrieve.IntentSymbolLookup {
		t.Errorf("intent=%s, want symbol_lookup", out.Intent)
	}
	if len(out.Symbols) == 0 {
		t.Fatal("want symbols")
	}
	if out.Symbols[0].QualifiedName != "Greet" {
		t.Errorf("symbol[0]=%s, want Greet", out.Symbols[0].QualifiedName)
	}
	if !strings.Contains(out.NextAction, "Read") {
		t.Errorf("next_action should be a Read directive: %q", out.NextAction)
	}
	// Without a graph indexed, avoid nudges toward `dex index`.
	// With graph indexed it would say "Do not grep". Either is acceptable
	// here; the symbol_lookup path is exercised either way.
	if !strings.Contains(out.Avoid, "Do not grep") && !strings.Contains(out.Avoid, "dex index") {
		t.Errorf("avoid should mention either don't-grep or index nudge: %q", out.Avoid)
	}
}

func TestContextRouterCallersAvoid(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Search() {}\nfunc UsesSearch() { Search() }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "callers of Search",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != retrieve.IntentCallers {
		t.Errorf("intent=%s, want callers", out.Intent)
	}
	// avoid branches on whether the references lane resolved usages.
	// With the calls-edge graph live (Go-only) or rg available, refs
	// should populate; otherwise we fall back to the Go-only-caveat
	// variant. Both reference the calls/refs concept.
	if out.Avoid == "" {
		t.Errorf("expected non-empty avoid for callers intent")
	}
	if !strings.Contains(out.Avoid, "calls") && !strings.Contains(out.Avoid, "references") {
		t.Errorf("avoid should mention `calls` or `references`: %q", out.Avoid)
	}
	// If rg is available, references should be populated for this fixture.
	if hasRg() && len(out.References) == 0 {
		t.Errorf("expected references for `Search` usages when rg is available; got 0")
	}
}

func hasRg() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

func TestContextRouterTruncatedReadFlagsNextAction(t *testing.T) {
	// Inline content has per-read caps (60 lines / 4 KB for targeted
	// intents). When a chunk exceeds those caps, Truncated=true is
	// set on the read — next_action must surface that so the agent
	// knows the inlined Content isn't the full chunk.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// Write a Go file with a function long enough to exceed the
	// 60-line targeted cap. Use 100 distinct printable lines so the
	// chunker treats it as one chunk.
	var body strings.Builder
	body.WriteString("package main\n\n// Long is intentionally long to exceed the inline cap.\nfunc Long() {\n")
	for i := 0; i < 100; i++ {
		body.WriteString(fmt.Sprintf("\t_ = %d\n", i))
	}
	body.WriteString("}\n")
	writeFile(t, filepath.Join(projDir, "long.go"), body.String())
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Long", // exact symbol match → symbol_lookup
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.SuggestedReads) == 0 {
		t.Fatal("expected suggested_reads")
	}
	if !out.SuggestedReads[0].Truncated {
		t.Fatalf("expected reads[0].Truncated; chunk was %d lines", out.SuggestedReads[0].EndLine-out.SuggestedReads[0].StartLine+1)
	}
	if !strings.Contains(out.NextAction, "truncated") {
		t.Errorf("next_action should mention truncation when reads[0].Truncated=true; got %q", out.NextAction)
	}
}

func TestContextRouterSymbolLookupMissNearMissCandidates(t *testing.T) {
	// When the user asks for a specific identifier via symbol_lookup
	// and we find nothing exact, the router should surface substring
	// matches in `hint` — mirroring what search_symbol does — so the
	// agent has real names to retry with instead of guessing.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Indexer() {}\nfunc IndexableExt() {}\nfunc cmdIndex() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "Index", // bare identifier; no exact match
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != retrieve.IntentSymbolLookup {
		t.Errorf("intent=%s, want symbol_lookup", out.Intent)
	}
	if len(out.Symbols) != 0 {
		t.Errorf("expected 0 symbol matches; got %d", len(out.Symbols))
	}
	if !strings.Contains(out.Hint, "did you mean") {
		t.Errorf("hint should surface candidates; got %q", out.Hint)
	}
	if !strings.Contains(out.Hint, "Indexer") && !strings.Contains(out.Hint, "IndexableExt") && !strings.Contains(out.Hint, "cmdIndex") {
		t.Errorf("hint should name at least one real candidate; got %q", out.Hint)
	}
}

func TestContextRouterNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "x.go"), "package x\n")

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "anything",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-index" {
		t.Errorf("status=%s, want no-index", out.Status)
	}
}

// An empty question is no longer an error — it routes to the session-start
// orientation path (#348 / #316 story 6). On an unindexed project it must
// degrade gracefully: intent "orient" with a hint that points at indexing,
// never the old "question is empty" error.
func TestContextRouterEmptyQuestionOrients(t *testing.T) {
	s := newServer("http://127.0.0.1:0", t.TempDir())
	_, out, err := s.ContextRouter(context.Background(), ContextInput{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "orient" {
		t.Errorf("intent=%q, want orient", out.Intent)
	}
	if out.Status == "ok" {
		t.Errorf("status=ok on an unindexed project, want a graceful degrade status; out=%+v", out)
	}
	if !strings.Contains(out.Hint, "index") {
		t.Errorf("hint should guide toward indexing, got %q", out.Hint)
	}
	if strings.Contains(out.Hint, "question is empty") {
		t.Errorf("empty question must orient, not error: %q", out.Hint)
	}
}

// On an indexed project with a community graph, an empty question returns the
// deterministic L0+L1 orientation bundle in out.Map (#348). Proves the router
// reaches codemap.RenderOrient end-to-end and names the indexed package.
func TestContextRouterEmptyQuestionRendersMap(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\ntype Store struct{}\nfunc (s *Store) Search() {}\nfunc (s *Store) Open() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedGraph(t, ctx, root, cacheDir)

	// Assign all three nodes to one community with PageRank so GraphCommunities
	// (min-members 3) returns a cluster the orient bundle can zoom into.
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GraphSetCentrality(ctx, []store.GraphCentralityRow{
		{ID: "m::p::type::Store", PageRank: 0.5, InDegree: 2, CommunityID: 1},
		{ID: "m::p::method::(*Store).Search", PageRank: 0.3, InDegree: 1, CommunityID: 1},
		{ID: "m::p::method::(*Store).Open", PageRank: 0.2, InDegree: 1, CommunityID: 1},
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(ctx, ContextInput{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "orient" {
		t.Fatalf("intent=%q, want orient", out.Intent)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Map == "" {
		t.Fatal("out.Map should hold the L0+L1 orientation bundle, got empty")
	}
	if !strings.Contains(out.Map, "p") {
		t.Errorf("orient bundle should name the indexed package; got:\n%s", out.Map)
	}
	// The map must equal a direct render of the same clusters — single-sourced
	// through codemap.RenderOrient, never a divergent reimplementation.
	comm, err := s.GraphCommunities(ctx, CommunitiesInput{MinMembers: 3, K: 50, TopK: 25, ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if want := codemap.RenderOrient(adaptCommunities(comm.Communities), codemap.DefaultL0Budget, codemap.DefaultL1Budget); out.Map != want {
		t.Errorf("out.Map diverges from codemap.RenderOrient:\ngot:\n%s\nwant:\n%s", out.Map, want)
	}
}

func TestCompactID(t *testing.T) {
	cases := []struct {
		name string
		n    graphquery.Node
		want string
	}{
		{"method", graphquery.Node{Kind: graph.NodeMethod, Name: "ContextRouter", QualifiedName: "(*Server).ContextRouter", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.(*Server).ContextRouter"},
		{"type", graphquery.Node{Kind: graph.NodeStruct, Name: "Server", QualifiedName: "Server", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.Server"},
		{"package", graphquery.Node{Kind: graph.NodePackage, QualifiedName: "github.com/foo/bar/internal/mcp", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp"},
		{"import", graphquery.Node{Kind: graph.NodeImport, Name: "sync", QualifiedName: "sync", PackagePath: "github.com/foo/bar/internal/mcp"}, "sync"},
		{"field", graphquery.Node{Kind: graph.NodeField, Name: "ChatClient", QualifiedName: "Server.ChatClient", PackagePath: "github.com/foo/bar/internal/mcp"}, "mcp.Server.ChatClient"},
		{"stdlib pkg path", graphquery.Node{Kind: graph.NodeFunction, Name: "Println", QualifiedName: "Println", PackagePath: "fmt"}, "fmt.Println"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retrieve.CompactID(tc.n); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// seedGraph writes a synthetic graph for `root` directly via the
// store's upsert methods. Avoids invoking ExtractGo (which needs a
// real go.mod + GOPATH-resolvable imports) so we can test the router's
// graph integration on a one-file fixture.
func seedGraph(t *testing.T, ctx context.Context, root, cacheDir string) {
	t.Helper()
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	typeID := "m::p::type::Store"
	methodID := "m::p::method::(*Store).Search"
	siblingID := "m::p::method::(*Store).Open"
	nodes := []store.GraphNodeRow{
		{ID: typeID, Kind: string(graph.NodeType), Name: "Store", QualifiedName: "Store",
			PackagePath: "p", FilePath: "main.go", StartLine: 1, EndLine: 5,
			MetadataJSON: []byte("{}"), ContentHash: "n1"},
		{ID: methodID, Kind: string(graph.NodeMethod), Name: "Search", QualifiedName: "(*Store).Search",
			PackagePath: "p", FilePath: "main.go", StartLine: 10, EndLine: 20,
			MetadataJSON: []byte("{}"), ContentHash: "n2"},
		{ID: siblingID, Kind: string(graph.NodeMethod), Name: "Open", QualifiedName: "(*Store).Open",
			PackagePath: "p", FilePath: "main.go", StartLine: 30, EndLine: 40,
			MetadataJSON: []byte("{}"), ContentHash: "n3"},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []store.GraphEdgeRow{
		{ID: "e1", Kind: string(graph.EdgeHasMethod), SrcID: typeID, DstID: methodID,
			FilePath: "main.go", StartLine: 10, EndLine: 20,
			MetadataJSON: []byte("{}"), ContentHash: "h1"},
		{ID: "e2", Kind: string(graph.EdgeHasMethod), SrcID: typeID, DstID: siblingID,
			FilePath: "main.go", StartLine: 30, EndLine: 40,
			MetadataJSON: []byte("{}"), ContentHash: "h2"},
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}
}

func TestContextRouterGraphSymbolLookup(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\ntype Store struct{}\nfunc (s *Store) Search() {}\nfunc (s *Store) Open() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	ctx := context.Background()
	seedGraph(t, ctx, root, cacheDir)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.ContextRouter(ctx, ContextInput{
		Question:    "Search",
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if out.Graph == nil || len(out.Graph.Nodes) == 0 {
		t.Fatalf("graph.nodes should be populated; got %+v", out.Graph)
	}

	var names, ids []string
	for _, n := range out.Graph.Nodes {
		names = append(names, n.QualifiedName)
		ids = append(ids, n.ID)
	}
	joinedNames := strings.Join(names, ",")
	for _, want := range []string{"Store", "(*Store).Search", "(*Store).Open"} {
		if !strings.Contains(joinedNames, want) {
			t.Errorf("graph.nodes should include %q; got %s", want, joinedNames)
		}
	}
	// IDs must be the compact form (`<pkg-tail>.<qualified-name>`),
	// never the legacy `<module>::<pkg>::<kind>::<qname>`.
	joinedIDs := strings.Join(ids, ",")
	if strings.Contains(joinedIDs, "::") {
		t.Errorf("graph.nodes[].id should be compact, not module-qualified; got %s", joinedIDs)
	}
	for _, want := range []string{"p.(*Store).Search", "p.(*Store).Open", "p.Store"} {
		if !strings.Contains(joinedIDs, want) {
			t.Errorf("graph.nodes[].id should include %q; got %s", want, joinedIDs)
		}
	}
	// Edges must reference the same compact IDs.
	for _, e := range out.Graph.Edges {
		if strings.Contains(e.From, "::") || strings.Contains(e.To, "::") {
			t.Errorf("edge id should be compact; got from=%q to=%q", e.From, e.To)
		}
	}
	if !strings.Contains(out.Avoid, "Do not grep") {
		t.Errorf("avoid should say don't grep when graph is indexed: %q", out.Avoid)
	}
}

func TestContextRouterKBudget(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "a.go"), "package x\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(projDir, "b.go"), "package x\n\nfunc B() {}\n")
	writeFile(t, filepath.Join(projDir, "c.go"), "package x\n\nfunc C() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.ContextRouter(context.Background(), ContextInput{
		Question:    "function",
		ProjectRoot: root,
		K:           2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
	}
	if len(out.SemanticHits) > 2 {
		t.Errorf("k=2 should cap semantic_hits; got %d", len(out.SemanticHits))
	}
}
