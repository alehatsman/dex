package mcp

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// #484: a hub symbol's impact must collapse the per-depth PageRank tail into a
// "+N more at depth D" line instead of dumping every node.
func TestCapPerDepth(t *testing.T) {
	// 3 nodes at depth 1, 4 at depth 2 (already sorted depth-asc, PR-desc).
	nodes := []graphquery.Reachable{
		{QualifiedName: "a1", Depth: 1, PageRank: 0.9},
		{QualifiedName: "a2", Depth: 1, PageRank: 0.5},
		{QualifiedName: "a3", Depth: 1, PageRank: 0.1},
		{QualifiedName: "b1", Depth: 2, PageRank: 0.4},
		{QualifiedName: "b2", Depth: 2, PageRank: 0.3},
		{QualifiedName: "b3", Depth: 2, PageRank: 0.2},
		{QualifiedName: "b4", Depth: 2, PageRank: 0.1},
	}

	kept, elided := capPerDepth(nodes, 2)

	if len(kept) != 4 { // 2 from depth 1 + 2 from depth 2
		t.Fatalf("kept = %d, want 4: %+v", len(kept), kept)
	}
	// Top-k by PageRank survive (slice was pre-sorted).
	want := []string{"a1", "a2", "b1", "b2"}
	for i, w := range want {
		if kept[i].QualifiedName != w {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i].QualifiedName, w)
		}
	}
	if len(elided) != 2 {
		t.Fatalf("elided = %+v, want 2 entries", elided)
	}
	if elided[0] != (DepthElision{Depth: 1, More: 1}) {
		t.Errorf("elided[0] = %+v, want {1 1}", elided[0])
	}
	if elided[1] != (DepthElision{Depth: 2, More: 2}) {
		t.Errorf("elided[1] = %+v, want {2 2}", elided[1])
	}
}

func TestCapPerDepthNoElisionUnderCap(t *testing.T) {
	nodes := []graphquery.Reachable{
		{QualifiedName: "a1", Depth: 1},
		{QualifiedName: "b1", Depth: 2},
	}
	kept, elided := capPerDepth(nodes, 8)
	if len(kept) != 2 {
		t.Errorf("kept = %d, want 2", len(kept))
	}
	if elided != nil {
		t.Errorf("elided = %+v, want nil", elided)
	}
}

// #485: a zero-caller exported method that satisfies an interface must produce
// a hint that names the interface, not a misleading silence.
func TestZeroCallerHintNamesInterface(t *testing.T) {
	// Server.Search (method) <-has_method- Server (type) -implements-> toolSurface
	view := &graphquery.View{
		NodesByID: map[string]graphquery.Node{
			"m": {ID: "m", Kind: graph.NodeMethod, Name: "Search"},
			"t": {ID: "t", Kind: graph.NodeType, Name: "Server"},
			"i": {ID: "i", Kind: graph.NodeType, Name: "toolSurface"},
		},
		EdgesBySrc: map[string][]graphquery.Edge{
			"t": {{Kind: graph.EdgeImplements, SrcID: "t", DstID: "i"}},
		},
		EdgesByDst: map[string][]graphquery.Edge{
			"m": {{Kind: graph.EdgeHasMethod, SrcID: "t", DstID: "m"}},
		},
	}
	targets := []graphquery.Node{view.NodesByID["m"]}

	got := zeroCallerHint(view, targets)
	if got == "" {
		t.Fatal("zeroCallerHint returned empty for an exported interface method")
	}
	if !strings.Contains(got, "toolSurface") {
		t.Errorf("hint does not name the interface: %q", got)
	}
}

// An exported symbol with no implements edge still gets the generic dispatch
// caveat (distinguish no-static-callers from dead).
func TestZeroCallerHintExportedGeneric(t *testing.T) {
	view := &graphquery.View{
		NodesByID:  map[string]graphquery.Node{"f": {ID: "f", Kind: graph.NodeFunction, Name: "Run"}},
		EdgesBySrc: map[string][]graphquery.Edge{},
		EdgesByDst: map[string][]graphquery.Edge{},
	}
	got := zeroCallerHint(view, []graphquery.Node{view.NodesByID["f"]})
	if got == "" {
		t.Fatal("want a generic exported-symbol hint, got empty")
	}
}

// An unexported symbol with zero callers is plausibly dead — no hint.
func TestZeroCallerHintUnexportedSilent(t *testing.T) {
	view := &graphquery.View{
		NodesByID:  map[string]graphquery.Node{"f": {ID: "f", Kind: graph.NodeFunction, Name: "search"}},
		EdgesBySrc: map[string][]graphquery.Edge{},
		EdgesByDst: map[string][]graphquery.Edge{},
	}
	if got := zeroCallerHint(view, []graphquery.Node{view.NodesByID["f"]}); got != "" {
		t.Errorf("unexported symbol should get no hint, got %q", got)
	}
}

// #486: callers should center the inlined snippet on the call site, not return
// the whole enclosing function body.
func TestCallSiteRange(t *testing.T) {
	hit := CallSite{StartLine: 78, EndLine: 229, CallSiteLine: 150}

	// Default: window centred on the call site.
	start, end := callSiteRange(hit, false, 6)
	if start != 144 || end != 156 {
		t.Errorf("windowed range = [%d,%d], want [144,156]", start, end)
	}

	// Verbose: full enclosing body.
	start, end = callSiteRange(hit, true, 6)
	if start != 78 || end != 229 {
		t.Errorf("verbose range = [%d,%d], want [78,229]", start, end)
	}

	// No call-site line: fall back to the whole body even when not verbose.
	start, end = callSiteRange(CallSite{StartLine: 10, EndLine: 40}, false, 6)
	if start != 10 || end != 40 {
		t.Errorf("no-callsite range = [%d,%d], want [10,40]", start, end)
	}

	// Window clamps to line 1 near the top of the file.
	start, _ = callSiteRange(CallSite{StartLine: 1, EndLine: 20, CallSiteLine: 3}, false, 6)
	if start != 1 {
		t.Errorf("clamped start = %d, want 1", start)
	}
}
