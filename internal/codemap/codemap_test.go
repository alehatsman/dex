package codemap

import (
	"strings"
	"testing"
)

func sym(name, kind, pkg, path string, line int, pr float64) Symbol {
	return Symbol{QualifiedName: name, Kind: kind, Pkg: pkg, Path: path, Line: line, PageRank: pr}
}

// three clusters of clearly different aggregate weight, for ordering tests.
func sampleClusters() []Cluster {
	return []Cluster{
		{ID: 1, Symbols: []Symbol{ // light
			sym("store.tiny", "func", "internal/store", "internal/store/x.go", 5, 0.01),
		}},
		{ID: 2, Symbols: []Symbol{ // heavy
			sym("mcp.Server.Handle", "method", "internal/mcp", "internal/mcp/server.go", 40, 0.50),
			sym("mcp.Server.Ask", "method", "internal/mcp", "internal/mcp/server.go", 80, 0.30),
			sym("eval.Run", "func", "internal/eval", "internal/eval/runner.go", 61, 0.05),
		}},
		{ID: 3, Symbols: []Symbol{ // medium
			sym("chunk.Chunk", "func", "internal/chunk", "internal/chunk/chunk.go", 10, 0.20),
		}},
	}
}

func TestRenderL0_RanksByAggregatePageRank(t *testing.T) {
	out := RenderL0(sampleClusters(), 1000)
	// Heavy cluster #2 must appear before medium #3 before light #1.
	i2, i3, i1 := strings.Index(out, "#2"), strings.Index(out, "#3"), strings.Index(out, "#1")
	if !(i2 >= 0 && i3 > i2 && i1 > i3) {
		t.Fatalf("clusters not ranked by weight (#2<#3<#1 expected):\n%s", out)
	}
}

func TestRenderL0_LabelsDominantPackage(t *testing.T) {
	out := RenderL0(sampleClusters(), 1000)
	// Cluster #2's dominant package (most PageRank) is internal/mcp.
	if !strings.Contains(out, "internal/mcp") {
		t.Fatalf("L0 should name the dominant package internal/mcp:\n%s", out)
	}
}

func TestRenderL0_CollapsesSamePackageClusters(t *testing.T) {
	// Two Louvain clusters whose dominant package is the same must read as
	// one headline (with both zoomable ids), not two near-identical rows
	// that crowd out other packages (#569).
	clusters := []Cluster{
		{ID: 10, Symbols: []Symbol{sym("mcp.Alpha", "func", "internal/mcp", "internal/mcp/a.go", 1, 0.90)}},
		{ID: 11, Symbols: []Symbol{sym("mcp.Beta", "func", "internal/mcp", "internal/mcp/b.go", 1, 0.80)}},
		{ID: 12, Symbols: []Symbol{sym("store.Gamma", "func", "internal/store", "internal/store/c.go", 1, 0.50)}},
	}
	out := RenderL0(clusters, 1000)
	if n := strings.Count(out, "internal/mcp"); n != 1 {
		t.Fatalf("same-package clusters must collapse to one internal/mcp line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "#10") || !strings.Contains(out, "#11") {
		t.Fatalf("collapsed line must list every zoomable cluster id:\n%s", out)
	}
	if !strings.Contains(out, "internal/store") {
		t.Fatalf("a distinct package must still appear on its own line:\n%s", out)
	}
}

func TestTopNames_FiltersNoiseRepresentatives(t *testing.T) {
	// err / id float up by PageRank but are useless representatives; the
	// meaningful names must win even though they rank lower (#569).
	syms := []Symbol{
		sym("p.err", "var", "p", "p/x.go", 1, 0.99),
		sym("p.id", "var", "p", "p/x.go", 2, 0.98),
		sym("p.Handler", "type", "p", "p/x.go", 3, 0.50),
		sym("p.Process", "func", "p", "p/x.go", 4, 0.40),
		sym("p.Resolve", "func", "p", "p/x.go", 5, 0.30),
	}
	got := strings.Join(topNames(syms, 3), ",")
	if got != "Handler,Process,Resolve" {
		t.Fatalf("noise representatives not filtered: got %q", got)
	}
}

func TestTopNames_BackfillsWhenAllNoise(t *testing.T) {
	// A cluster of only short/noise names should still surface something
	// rather than an empty representative list.
	syms := []Symbol{
		sym("p.err", "var", "p", "p/x.go", 1, 0.9),
		sym("p.id", "var", "p", "p/x.go", 2, 0.8),
	}
	if got := topNames(syms, 3); len(got) != 2 {
		t.Fatalf("backfill should surface all-noise names rather than nothing: %v", got)
	}
}

func TestRenderL0_HonorsBudget(t *testing.T) {
	// A tiny budget shows at least one cluster and reports the rest dropped.
	out := RenderL0(sampleClusters(), 1)
	if !strings.Contains(out, "#2") {
		t.Fatalf("smallest budget must still show the top cluster:\n%s", out)
	}
	if !strings.Contains(out, "more cluster") {
		t.Fatalf("dropped clusters must be reported:\n%s", out)
	}
}

func TestRenderL0_Empty(t *testing.T) {
	out := RenderL0(nil, 150)
	if !strings.Contains(out, "no clusters") {
		t.Fatalf("empty input should explain itself:\n%s", out)
	}
}

func TestRenderL1_GroupsByPackageAndNamesFiles(t *testing.T) {
	c := Cluster{ID: 2, Symbols: []Symbol{
		sym("mcp.Server.Handle", "method", "internal/mcp", "internal/mcp/server.go", 40, 0.50),
		sym("eval.Run", "func", "internal/eval", "internal/eval/runner.go", 61, 0.05),
	}}
	out := RenderL1(c, 1000)
	// Both package headers and the file:line locator (Phase-B ruler needs the
	// package named and the file reachable).
	for _, want := range []string{"## internal/mcp", "## internal/eval", "internal/mcp/server.go:40", "internal/eval/runner.go:61"} {
		if !strings.Contains(out, want) {
			t.Fatalf("L1 missing %q:\n%s", want, out)
		}
	}
	// internal/mcp (heavier) must come before internal/eval.
	if strings.Index(out, "## internal/mcp") > strings.Index(out, "## internal/eval") {
		t.Fatalf("L1 packages not ordered by PageRank:\n%s", out)
	}
}

func TestDominantPkg_DeterministicTieBreak(t *testing.T) {
	// Equal weight + equal count -> lexicographically smaller package wins.
	syms := []Symbol{
		sym("b.Foo", "func", "zpkg", "z.go", 1, 0.1),
		sym("a.Bar", "func", "apkg", "a.go", 1, 0.1),
	}
	if got := dominantPkg(syms); got != "apkg" {
		t.Fatalf("tie-break: got %q, want apkg", got)
	}
}

func TestSize_PrefersTrueCommunitySize(t *testing.T) {
	// Only 2 symbols fetched but the community truly has 40 members.
	c := Cluster{ID: 1, Size: 40, Symbols: []Symbol{
		sym("a.Foo", "func", "pkg", "a.go", 1, 0.2),
		sym("a.Bar", "func", "pkg", "a.go", 2, 0.1),
	}}
	if got := c.size(); got != 40 {
		t.Fatalf("size: got %d, want 40", got)
	}
	if !strings.Contains(RenderL1(c, 1000), "(40 symbols)") {
		t.Fatalf("L1 should report true size 40:\n%s", RenderL1(c, 1000))
	}
	// Size unset (0) falls back to the fetched count.
	c2 := Cluster{ID: 2, Symbols: []Symbol{sym("x.Y", "func", "p", "x.go", 1, 0.1)}}
	if got := c2.size(); got != 1 {
		t.Fatalf("fallback size: got %d, want 1", got)
	}
}

func TestShortName(t *testing.T) {
	cases := map[string]string{
		"github.com/x/y.Foo": "Foo",
		"pkg.Bar":            "Bar",
		"Baz":                "Baz",
	}
	for in, want := range cases {
		if got := shortName(in); got != want {
			t.Fatalf("shortName(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestShownL0_WeightOrderedAndComplete(t *testing.T) {
	shown := ShownL0(sampleClusters(), 1000)
	if len(shown) != 3 {
		t.Fatalf("generous budget should show all 3 clusters, got %d", len(shown))
	}
	if shown[0].ID != 2 || shown[1].ID != 3 || shown[2].ID != 1 {
		t.Fatalf("not weight-ranked: got ids %d,%d,%d (want 2,3,1)", shown[0].ID, shown[1].ID, shown[2].ID)
	}
}

func TestShownL0_BudgetTruncatesAgreesWithRenderL0(t *testing.T) {
	cs := sampleClusters()
	// A tiny budget shows only the heaviest cluster; RenderL0 must then report
	// the other two as dropped — proving the two are single-sourced.
	shown := ShownL0(cs, 1)
	if len(shown) != 1 || shown[0].ID != 2 {
		t.Fatalf("tiny budget: want only cluster #2, got %d clusters", len(shown))
	}
	if out := RenderL0(cs, 1); !strings.Contains(out, "2 more cluster(s)") {
		t.Fatalf("RenderL0 should note 2 dropped clusters:\n%s", out)
	}
}

// RenderOrient must compose the L0 overview followed by an L1 zoom into the
// most-central cluster — the session-start orientation bundle (#348).
func TestRenderOrient_ComposesL0PlusTopClusterL1(t *testing.T) {
	cs := sampleClusters()
	bundle := RenderOrient(cs, nil, 1000, 1000)

	// L0 overview leads the bundle verbatim.
	l0 := RenderL0(cs, 1000)
	if !strings.HasPrefix(bundle, l0) {
		t.Fatalf("orient bundle must start with the L0 overview:\n%s", bundle)
	}
	// The L1 zoom of the top-ranked cluster (#2, internal/mcp) follows verbatim.
	top := ShownL0(cs, 1000)[0]
	if top.ID != 2 {
		t.Fatalf("top cluster should be the heaviest (#2), got #%d", top.ID)
	}
	if l1 := RenderL1(top, 1000); !strings.Contains(bundle, l1) {
		t.Fatalf("orient bundle must contain the L1 zoom of the top cluster:\n%s", bundle)
	}
}

// The bundle must be byte-stable across calls so injected orientation stays
// cache-friendly for a session (#348).
func TestRenderOrient_Deterministic(t *testing.T) {
	cs := sampleClusters()
	if a, b := RenderOrient(cs, nil, 1000, 1000), RenderOrient(cs, nil, 1000, 1000); a != b {
		t.Fatalf("orient bundle not byte-stable across calls:\n%q\n!=\n%q", a, b)
	}
}

// With no clusters there is nothing to zoom — orient degrades to L0 alone and
// must not panic.
func TestRenderOrient_EmptyDegradesToL0(t *testing.T) {
	if got, want := RenderOrient(nil, nil, 150, 1000), RenderL0(nil, 150); got != want {
		t.Fatalf("empty orient should equal L0 alone:\ngot  %q\nwant %q", got, want)
	}
}
