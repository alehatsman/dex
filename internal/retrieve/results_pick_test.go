package retrieve

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// demoteDocBuild is the test's path classifier — the policy the transport
// injects as isNonImplPath in production. It demotes docs and build/config
// files so the code-preference tiebreaker cases below have something to bite on.
func demoteDocBuild(p string) bool {
	return strings.HasSuffix(p, ".md") ||
		strings.HasSuffix(p, ".yml") ||
		strings.HasSuffix(p, ".yaml")
}

// The cases below moved down from internal/mcp when #114 deleted the
// pickSuggestedReads transport wrapper: the ranking policy lives here, so the
// tests call PickSuggestedReads directly, supplying demoteDocBuild for the
// classifier the wrapper used to inject.

func TestPickSuggestedReadsSymbolIntent(t *testing.T) {
	syms := []SymbolHit{
		{QualifiedName: "Foo", Path: "a.go", StartLine: 10, EndLine: 20},
		{QualifiedName: "Bar", Path: "b.go", StartLine: 5, EndLine: 15},
	}
	got := PickSuggestedReads(IntentSymbolLookup, nil, syms, nil, nil, demoteDocBuild)
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

	got := PickSuggestedReads(IntentBehaviorSearch, sem, nil, symbolPaths, nil, demoteDocBuild)
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
	got := PickSuggestedReads(IntentBehaviorSearch, sem, nil, nil, nil, demoteDocBuild)
	if len(got) == 0 || got[0].Path != "README.md" {
		t.Errorf("behavior_search: higher-scoring doc should win; got %+v", got)
	}

	// Architecture — same: README wins by score.
	gotArch := PickSuggestedReads(IntentArchitecture, sem, nil, nil, nil, demoteDocBuild)
	if len(gotArch) == 0 || gotArch[0].Path != "README.md" {
		t.Errorf("architecture should keep README on top; got %+v", gotArch)
	}

	// Other code-oriented intents still apply the tiebreaker.
	gotEdit := PickSuggestedReads(IntentEditingContext, sem, nil, nil, nil, demoteDocBuild)
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
	got := PickSuggestedReads(IntentEditingContext, sem, nil, nil, nil, demoteDocBuild)
	if len(got) == 0 || got[0].Path != "internal/mcp/server.go" {
		t.Errorf("editing_context should prefer .go over Taskfile.yml; got %+v", got)
	}
	gotArch := PickSuggestedReads(IntentArchitecture, sem, nil, nil, nil, demoteDocBuild)
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
		got := PickSuggestedReads(IntentArchitecture, sem, nil, nil, view, demoteDocBuild)
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
		got := PickSuggestedReads(IntentArchitecture, sem, nil, nil, view, demoteDocBuild)
		if len(got) == 0 || got[0].Path != "ordinary.go" {
			t.Errorf("score-bucket gap should keep ordinary.go on top; got %+v", got)
		}
	})

	t.Run("package_topology also uses centrality", func(t *testing.T) {
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := PickSuggestedReads(IntentPackageTopology, sem, nil, nil, view, demoteDocBuild)
		if len(got) == 0 || got[0].Path != "hub.go" {
			t.Errorf("package_topology should also reorder by PageRank; got %+v", got)
		}
	})

	t.Run("behavior_search ignores PageRank — strict score order", func(t *testing.T) {
		sem := []SemHit{
			{Path: "ordinary.go", StartLine: 1, EndLine: 10, Score: 0.58},
			{Path: "hub.go", StartLine: 1, EndLine: 10, Score: 0.55},
		}
		got := PickSuggestedReads(IntentBehaviorSearch, sem, nil, nil, view, demoteDocBuild)
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
		got := PickSuggestedReads(IntentArchitecture, sem, nil, nil, nil, demoteDocBuild)
		if len(got) == 0 || got[0].Path != "ordinary.go" {
			t.Errorf("nil view should fall back to score order; got %+v", got)
		}
	})
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
	got := PickSuggestedReads(IntentArchitecture, sem, nil, nil, nil, demoteDocBuild)
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
	got := PickSuggestedReads(IntentArchitecture, sem, nil, nil, nil, demoteDocBuild)
	// Exploration intents widen to 5 reads so the initial bundle gives
	// the caller a real cross-file picture.
	if len(got) != 5 {
		t.Errorf("architecture should return 5 reads, got %d", len(got))
	}
}
