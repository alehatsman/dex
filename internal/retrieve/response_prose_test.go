package retrieve

import (
	"strings"
	"testing"
)

// Migrated from the mcp transport wrappers (#95a tail fold): the prose builders
// live here, so the table tests do too — driven directly over the neutral types.

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
		{IntentSymbolLookup, reads, syms, 0.8, 0, 0, false, "Read x.go lines 10-30"},
		// symbol_lookup ambiguous: 3 symbols across 3 distinct paths —
		// next_action must signal the count, not say "the definition".
		{IntentSymbolLookup, reads, []SymbolHit{
			{QualifiedName: "Options", Path: "a.go"},
			{QualifiedName: "Options", Path: "b.go"},
			{QualifiedName: "Options", Path: "c.go"},
		}, 0.8, 0, 0, false, "3 definitions across files"},
		// symbol_lookup with NO symbols but a strong semantic hit: next_action
		// must not claim "the definition" — that lied when the symbol genuinely
		// wasn't found. Soft fallback.
		{IntentSymbolLookup, reads, nil, 0.8, 0, 0, false, "No exact symbol match"},
		{IntentEditingContext, reads, syms, 0.8, 0, 0, false, "before editing"},
		{IntentBehaviorSearch, reads, syms, 0.8, 0, 0, false, "ground your answer"},
		{IntentArchitecture, reads, syms, 0.8, 0, 0, false, "structural overview"},
		// callers with graph edges: directive points at the graph lane.
		{IntentCallers, nil, syms, 0.8, 4, 0, false, "4 callers edges"},
		{IntentCallers, nil, syms, 0.8, 1, 0, false, "1 callers edge"}, // singular
		// callers without graph edges but with rg-backed references.
		{IntentCallers, nil, syms, 0.8, 0, 3, false, "3 call sites"},
		{IntentCallers, nil, syms, 0.8, 0, 1, false, "1 call site"},
		// callers with neither: soft "start from the symbol" line.
		{IntentCallers, nil, syms, 0.8, 0, 0, false, "No callers found"},
		{IntentSymbolLookup, nil, nil, 0, 0, 0, false, "Rephrase"},
		// Low-confidence: no symbols and top semantic score below the threshold
		// route to the "weak match" branch instead of a noise hit.
		{IntentBehaviorSearch, reads, nil, 0.30, 0, 0, false, "Top semantic match is weak"},
		// Symbols present — confidence comes from the structural lane, so the
		// low-score branch must NOT trigger.
		{IntentBehaviorSearch, reads, syms, 0.30, 0, 0, false, "ground your answer"},
		// package_topology with graph edges: weak-score branch must NOT fire.
		{IntentPackageTopology, reads, nil, 0.30, 47, 0, false, "graph.edges"},
		{IntentPackageTopology, reads, nil, 0.30, 47, 0, false, "47 imports"},
		// editing_context with blame: weak-score branch must NOT fire.
		{IntentEditingContext, reads, nil, 0.30, 0, 0, true, "before editing"},
		// package_topology with empty graph + low scores → genuine no-signal.
		{IntentPackageTopology, reads, nil, 0.30, 0, 0, false, "Top semantic match is weak"},
		// assemble (#725): the working set IS the answer.
		{IntentAssemble, reads, syms, 0.8, 0, 0, false, "Working set assembled"},
	}
	for _, tc := range cases {
		t.Run(tc.intent+" "+tc.want, func(t *testing.T) {
			got := BuildNextAction(tc.intent, tc.reads, tc.syms, tc.topSem, tc.graphEdges, tc.refs, tc.hasBlame)
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
		{"callers warns non-Go fallback", IntentCallers, sem, syms, true, "name-based (tree-sitter)"},
		{"callers without graph still warns", IntentCallers, sem, syms, false, "name-based (tree-sitter)"},
		{"symbol_lookup without graph nudges to index", IntentSymbolLookup, sem, syms, false, "Run `dex index"},
		{"symbol_lookup with graph: don't grep", IntentSymbolLookup, sem, syms, true, "Do not grep"},
		{"behavior + both lanes", IntentBehaviorSearch, sem, syms, true, "Do not grep for the identifier"},
		{"behavior + symbols only", IntentBehaviorSearch, nil, syms, true, "Do not grep for the identifier"},
		{"behavior + semantic only", IntentBehaviorSearch, sem, nil, true, "Do not read entire files"},
		{"behavior + nothing", IntentBehaviorSearch, nil, nil, true, ""},
		{"behavior without graph nudges to index", IntentBehaviorSearch, sem, syms, false, "Run `dex index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildAvoid(tc.intent, tc.sem, tc.syms, tc.graphIndexed, false)
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

func TestMaxSemScore(t *testing.T) {
	// semantic_hits is reordered by summary merging and rerank, so reading
	// hits[0] mis-classified strong responses when a low-score symbol-driven
	// entry was promoted to the head — must scan all hits.
	hits := []SemHit{
		{Path: "noise.go", Score: 0.01},
		{Path: "strong.go", Score: 0.85},
		{Path: "mid.go", Score: 0.42},
	}
	if got := maxSemScore(hits); got != 0.85 {
		t.Errorf("maxSemScore = %v, want 0.85 (must scan all hits, not just [0])", got)
	}
	if got := maxSemScore(nil); got != 0 {
		t.Errorf("empty hits should yield 0; got %v", got)
	}
}
