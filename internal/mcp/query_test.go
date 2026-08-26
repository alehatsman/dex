package mcp

import "testing"

// TestClassifyQuery walks the precision ladder (spec specs/two-verb-surface.md):
// input shape → lane, output precision tracks input precision. This is the S2
// router-accuracy corpus — the gate that the ask+look merge does not misroute.
func TestClassifyQuery(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		kind     string
		wantLane string
		wantDet  string
	}{
		// Rung 0 — literal artifact fetch (exact lanes, zero inference).
		{"file path", "internal/mcp/server.go", "", "read", "path"},
		{"location", "server.go:829", "", "locate", "location"},
		{"line range", "server.go:120-140", "", "read", "location"}, // a slice via read/lines mode
		{"regex", "/func .*Verb/", "", "grep", "regex"},

		// Rung 1 — named symbol → graph (NARROW DEFAULT, no fusion).
		{"bare symbol", "NewServer", "", "symbol", "symbol"},
		{"receiver symbol", "(*Server).Run", "", "symbol", "symbol"},
		{"package symbol", "mcp.NewServer", "", "symbol", "symbol"},
		{"snake symbol", "resolve_intent", "", "symbol", "symbol"},

		// Rung 2+ — prose → semantic/intent lane.
		{"behavior question", "how are edits debounced?", "", "semantic", "prose"},
		{"where question", "where do we validate the token", "", "semantic", "prose"},
		{"architecture question", "how does indexing work", "", "semantic", "prose"},
		{"two words no q", "token validation", "", "semantic", "prose"},
		// #229: prose mentioning a slash-separated term must still route to
		// semantic, not be misread as a literal (whole-sentence) file path.
		{"prose with slash", "how does since:/diff: resolve a ref into changed symbols", "", "semantic", "prose"},

		// kind overrides win outright and are marked forced.
		{"force search on a symbol-shaped input", "flush", "search", "semantic", "forced"},
		{"force read", "somefile", "read", "read", "forced"},
		{"force impact", "NewServer", "impact", "symbol", "forced"},
		{"force architecture", "the watcher", "architecture", "semantic", "forced"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lr, _, det, _ := classifyQuery(c.input, c.kind)
			if lr.lane != c.wantLane {
				t.Errorf("lane = %q, want %q", lr.lane, c.wantLane)
			}
			if det != c.wantDet {
				t.Errorf("detected = %q, want %q", det, c.wantDet)
			}
		})
	}
}

// TestClassifyQueryNarrowDefault locks the core guarantee: a bare symbol routes
// to the graph lane and offers search as the road not taken, never the reverse.
func TestClassifyQueryNarrowDefault(t *testing.T) {
	lr, _, det, alt := classifyQuery("Flush", "")
	if lr.lane != "symbol" || det != "symbol" {
		t.Fatalf("bare symbol should route to graph lane, got lane=%q det=%q", lr.lane, det)
	}
	if len(alt) != 1 || alt[0].Kind != "search" {
		t.Fatalf("symbol lane must offer search as the alt, got %+v", alt)
	}
}

// TestForcedKindDirection checks that a graph-direction kind carries its
// direction through to the lane route.
func TestForcedKindDirection(t *testing.T) {
	for _, tc := range []struct{ kind, dir string }{
		{"callers", "callers"}, {"callees", "callees"},
		{"impact", "impact"}, {"path", "path"},
	} {
		lr, _, _, _ := classifyQuery("NewServer", tc.kind)
		if lr.lane != "symbol" || lr.direction != tc.dir {
			t.Errorf("kind=%q → lane=%q dir=%q, want symbol/%s", tc.kind, lr.lane, lr.direction, tc.dir)
		}
	}
}

// assertLiveNextSteps enforces the clean-break invariant now guaranteed at the
// source rather than by a rewrite pass (#207 deleted normalizeNext): a next-step
// must name a verb that still exists on the surface and use the merged arg key.
func assertLiveNextSteps(t *testing.T, steps []NextStep) {
	t.Helper()
	deadVerbs := map[string]bool{"look": true, "ask": true, "remember": true}
	for _, s := range steps {
		if deadVerbs[s.Verb] {
			t.Errorf("emitter produced dead verb %q (Args %v) — emit query/record at the source", s.Verb, s.Args)
		}
		for _, k := range []string{"target", "question"} {
			if s.Args != nil && s.Args[k] != nil {
				t.Errorf("emitter produced pre-merge arg key %q — use input=…: %+v", k, s)
			}
		}
	}
}

func TestLooksLikeSymbol(t *testing.T) {
	yes := []string{"Foo", "foo_bar", "(*Server).Run", "Server.Run", "mcp.NewServer", "a.b.c", "T::method"}
	no := []string{"how are you", "where is x", "foo bar", "what?", "is this a symbol?", "", "  "}
	for _, s := range yes {
		if !looksLikeSymbol(s) {
			t.Errorf("looksLikeSymbol(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeSymbol(s) {
			t.Errorf("looksLikeSymbol(%q) = true, want false", s)
		}
	}
}

// TestKindToLaneCoverage asserts every advertised kind maps to a lane — no
// capability from the 11 intents + look lanes is unreachable via kind.
func TestKindToLaneCoverage(t *testing.T) {
	kinds := []string{
		"read", "grep", "locate",
		"symbol", "callers", "callees", "impact", "path",
		"search", "editing", "assemble", "architecture", "packages", "orient", "review",
	}
	for _, k := range kinds {
		if _, ok := kindToLane(k); !ok {
			t.Errorf("kind %q does not map to a lane", k)
		}
	}
	if _, ok := kindToLane("bogus"); ok {
		t.Error("unknown kind should not map to a lane")
	}
}
