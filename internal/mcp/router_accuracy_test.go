package mcp

import (
	"testing"
)

// This is #110's router-accuracy gate (internal/retrieve/router_accuracy_test.go)
// re-pointed at the two-verb surface (#195 S5). #110 scored the prose→intent
// decision of retrieve.ResolveIntent; the two-verb collapse moved the *first*
// routing decision up a level — into query's shape classifier (classifyQuery),
// which decides which LANE an input's shape belongs to before any intent routing
// runs. So the cutover gate scores shape→lane here, while retrieve's harness
// still guards the semantic lane's internal prose→intent sub-routing. Together
// they cover the whole ladder end to end.
//
// Unlike retrieve's keyword-scored intent router (fuzzy → ratchet FLOOR),
// classifyQuery is a deterministic regex/string decision, so the gate is exact:
// every rung must route to its labelled lane. A regression that reroutes any
// shape trips the gate loudly instead of silently eroding an accuracy percentage.

// routeCase is one labelled input in the shape→lane corpus.
type routeCase struct {
	input    string // raw query string
	kind     string // optional explicit kind override (empty = infer from shape)
	wantLane string // lane classifyQuery must choose: read|grep|locate|symbol|semantic
	rung     string // the precision-ladder rung it exercises (for legible failures)
}

// routerShapeCorpus spans every rung of the precision ladder (spec §"the
// precision ladder"): literal path, path range, path:line location, /regex/,
// bare/qualified/package/path-qualified symbols, multi-word prose across the
// behavior/edit/architecture/topology/orient/review intents, and the four
// kind-forced overrides. A single-token word is a symbol by design (narrow
// default: naming a symbol earns its call graph); prose requires more than one
// token — those two neighbouring cases pin the one genuinely fuzzy boundary.
var routerShapeCorpus = []routeCase{
	// literal path → read (compressed signatures)
	{input: "internal/mcp/server.go", wantLane: "read", rung: "literal-path"},
	{input: "cmd/dex/main.go", wantLane: "read", rung: "literal-path"},
	{input: "README.md", wantLane: "read", rung: "literal-path"},
	{input: "internal/mcp/query_test.go", wantLane: "read", rung: "literal-path"},

	// path range (path:N-M) → read lane, lines mode
	{input: "internal/mcp/server.go:120-140", wantLane: "read", rung: "path-range"},
	{input: "query.go:10-20", wantLane: "read", rung: "path-range"},

	// path:line → locate (single-line pointer)
	{input: "internal/mcp/query.go:120", wantLane: "locate", rung: "path-line"},
	{input: "server.go:42", wantLane: "locate", rung: "path-line"},

	// /regex/ → grep
	{input: "/func .*Verb/", wantLane: "grep", rung: "regex"},
	{input: "/TODO|FIXME/", wantLane: "grep", rung: "regex"},
	{input: "/^type [A-Za-z]+ struct/", wantLane: "grep", rung: "regex"},

	// bare / qualified / package / path-qualified symbols → graph lane
	{input: "NewServer", wantLane: "symbol", rung: "bare-symbol"},
	{input: "queryVerb", wantLane: "symbol", rung: "bare-symbol"},
	{input: "classifyQuery", wantLane: "symbol", rung: "bare-symbol"},
	{input: "(*Server).Run", wantLane: "symbol", rung: "receiver-symbol"},
	{input: "Server.Run", wantLane: "symbol", rung: "qualified-symbol"},
	{input: "mcp.NewServer", wantLane: "symbol", rung: "package-symbol"},
	{input: "std::vector::push_back", wantLane: "symbol", rung: "path-qualified-symbol"},

	// multi-word prose → semantic (intent sub-routing is retrieve's harness)
	{input: "how are edits debounced?", wantLane: "semantic", rung: "behavior-prose"},
	{input: "where is the retry logic", wantLane: "semantic", rung: "behavior-prose"},
	{input: "add a flag to disable the cache", wantLane: "semantic", rung: "edit-prose"},
	{input: "what is the overall architecture", wantLane: "semantic", rung: "architecture-prose"},
	{input: "how do the packages depend on each other", wantLane: "semantic", rung: "topology-prose"},
	{input: "review my working tree changes", wantLane: "semantic", rung: "review-prose"},
	{input: "explain the indexing pipeline", wantLane: "semantic", rung: "behavior-prose"},
	{input: "why does the store use a single writer", wantLane: "semantic", rung: "behavior-prose"},

	// the fuzzy boundary, pinned: one word is a symbol, its multi-word gloss is prose
	{input: "debounce", wantLane: "symbol", rung: "single-token-is-symbol"},
	{input: "how does debounce work", wantLane: "semantic", rung: "single-token-gloss-is-prose"},

	// explicit kind wins outright, regardless of shape
	{input: "anything at all", kind: "grep", wantLane: "grep", rung: "forced-grep"},
	{input: "Foo", kind: "search", wantLane: "semantic", rung: "forced-search-over-symbol"},
	{input: "internal/x.go", kind: "review", wantLane: "semantic", rung: "forced-review-over-path"},
	{input: "NewServer", kind: "callers", wantLane: "symbol", rung: "forced-graph-direction"},
	{input: "how are edits debounced", kind: "read", wantLane: "read", rung: "forced-read-over-prose"},
}

// TestQueryRouterAccuracy is the S5 cutover gate: every rung of the precision
// ladder routes to its labelled lane through query's shape classifier. Because
// classifyQuery is deterministic the bar is 100% — any miss is a routing
// regression and fails loud, naming the rung, the input, and both lanes.
func TestQueryRouterAccuracy(t *testing.T) {
	if len(routerShapeCorpus) == 0 {
		t.Fatal("empty corpus")
	}
	var misses int
	seenRungs := map[string]bool{}
	for _, c := range routerShapeCorpus {
		seenRungs[c.rung] = true
		lr, _, detected, _ := classifyQuery(c.input, c.kind)
		if lr.lane != c.wantLane {
			misses++
			t.Errorf("[%s] classifyQuery(%q, kind=%q) → lane=%q detected=%q; want lane=%q",
				c.rung, c.input, c.kind, lr.lane, detected, c.wantLane)
		}
	}
	accuracy := float64(len(routerShapeCorpus)-misses) / float64(len(routerShapeCorpus))
	t.Logf("shape→lane accuracy: %.1f%% (%d/%d cases, %d rungs)",
		accuracy*100, len(routerShapeCorpus)-misses, len(routerShapeCorpus), len(seenRungs))

	// Every lane the classifier can emit must be exercised by the corpus, so the
	// gate can't silently stop covering a lane as the ladder evolves.
	for _, lane := range []string{"read", "grep", "locate", "symbol", "semantic"} {
		found := false
		for _, c := range routerShapeCorpus {
			if c.wantLane == lane {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("corpus never exercises lane %q; add a case", lane)
		}
	}
}
