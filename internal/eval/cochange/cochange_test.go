package cochange

import (
	"testing"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
)

// mkView builds a graph view from (nodeID → filePath) and a list of directed
// call edges (by node ID). NodesByPath and EdgesByKind are populated the way
// Compute reads them.
func mkView(nodeFiles map[string]string, calls [][2]string) *graphquery.View {
	v := &graphquery.View{
		NodesByID:   map[string]graphquery.Node{},
		NodesByPath: map[string][]graphquery.Node{},
		EdgesByKind: map[graph.EdgeKind][]graphquery.Edge{},
	}
	for id, fp := range nodeFiles {
		n := graphquery.Node{ID: id, FilePath: fp}
		v.NodesByID[id] = n
		v.NodesByPath[fp] = append(v.NodesByPath[fp], n)
	}
	for _, c := range calls {
		v.EdgesByKind[graph.EdgeCalls] = append(v.EdgesByKind[graph.EdgeCalls],
			graphquery.Edge{Kind: graph.EdgeCalls, SrcID: c[0], DstID: c[1]})
	}
	return v
}

func q(anchor string, gold ...string) eval.GoldenQuery {
	return eval.GoldenQuery{ID: "c:" + anchor, Anchor: anchor, RelevantFiles: gold}
}

func TestCompute_OneAndTwoHop(t *testing.T) {
	// a.go → b.go → c.go (calls). a.go has no edge to c.go directly.
	view := mkView(
		map[string]string{"na": "a.go", "nb": "b.go", "nc": "c.go", "nd": "d.go"},
		[][2]string{{"na", "nb"}, {"nb", "nc"}},
	)
	queries := []eval.GoldenQuery{
		q("a.go", "b.go"), // 1-hop
		q("a.go", "c.go"), // 2-hop only
		q("a.go", "d.go"), // unreachable
	}
	rep := Compute(view, queries, "go")

	if rep.Queries != 3 || rep.SrcOnly != 3 || rep.TestGold != 0 {
		t.Fatalf("counts: queries=%d src=%d test=%d, want 3/3/0", rep.Queries, rep.SrcOnly, rep.TestGold)
	}
	if rep.OneHop != 1 {
		t.Errorf("OneHop = %d, want 1 (only a→b)", rep.OneHop)
	}
	if rep.TwoHop != 2 {
		t.Errorf("TwoHop = %d, want 2 (a→b and a→b→c)", rep.TwoHop)
	}
	if rep.AnchorInGraph != 3 {
		t.Errorf("AnchorInGraph = %d, want 3", rep.AnchorInGraph)
	}
}

func TestCompute_TestGoldExcluded(t *testing.T) {
	view := mkView(map[string]string{"na": "a.go", "nb": "b.go"}, [][2]string{{"na", "nb"}})
	queries := []eval.GoldenQuery{
		q("a.go", "b.go"),              // src-only, 1-hop
		q("a.go", "a_test.go"),         // test-tainted → excluded
		q("a.go", "b.go", "x_test.go"), // any test file taints the whole query
	}
	rep := Compute(view, queries, "go")
	if rep.Queries != 3 {
		t.Fatalf("Queries = %d, want 3", rep.Queries)
	}
	if rep.TestGold != 2 {
		t.Errorf("TestGold = %d, want 2", rep.TestGold)
	}
	if rep.SrcOnly != 1 || rep.OneHop != 1 {
		t.Errorf("SrcOnly=%d OneHop=%d, want 1/1", rep.SrcOnly, rep.OneHop)
	}
}

func TestCompute_AnchorNotInGraph(t *testing.T) {
	// Anchor file has no extracted nodes: unresolved, and unreachable.
	view := mkView(map[string]string{"nb": "b.go", "nc": "c.go"}, [][2]string{{"nb", "nc"}})
	rep := Compute(view, []eval.GoldenQuery{q("a.go", "b.go")}, "go")
	if rep.SrcOnly != 1 || rep.AnchorInGraph != 0 {
		t.Fatalf("SrcOnly=%d AnchorInGraph=%d, want 1/0", rep.SrcOnly, rep.AnchorInGraph)
	}
	if rep.OneHop != 0 || rep.TwoHop != 0 {
		t.Errorf("reach: OneHop=%d TwoHop=%d, want 0/0", rep.OneHop, rep.TwoHop)
	}
}

func TestCompute_NilView(t *testing.T) {
	rep := Compute(nil, []eval.GoldenQuery{q("a.go", "b.go")}, "go")
	if rep.SrcOnly != 1 || rep.OneHop != 0 || rep.TwoHop != 0 {
		t.Errorf("nil view: %+v, want src=1 reach=0", rep)
	}
}

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		path, lang string
		want       bool
	}{
		{"foo_test.go", "go", true},
		{"foo.go", "go", false},
		{"tests/test_app.py", "python", true},
		{"src/flask/app.py", "python", false},
		{"conftest.py", "python", true},
		{"crates/matcher/tests/test_matcher.rs", "rust", true},
		{"crates/cli/src/lib.rs", "rust", false},
		{"src/test/java/FooTest.java", "java", true},
		{"base/CharMatcher.java", "java", false},
		{"x/foo.test.ts", "typescript", true},
		{"x/foo.ts", "typescript", false},
	}
	for _, c := range cases {
		if got := isTestFile(c.path, c.lang); got != c.want {
			t.Errorf("isTestFile(%q,%q) = %v, want %v", c.path, c.lang, got, c.want)
		}
	}
}

func TestDrift(t *testing.T) {
	base := NewSuite([]Cell{
		{Repo: "flask", Report: Report{SrcOnly: 100, TwoHop: 43}},
		{Repo: "ripgrep", Report: Report{SrcOnly: 100, TwoHop: 0}},
	})
	// flask holds, ripgrep jumps 0 → 0.30 (a resolver change reconnecting pairs).
	cur := NewSuite([]Cell{
		{Repo: "flask", Report: Report{SrcOnly: 100, TwoHop: 44}},
		{Repo: "ripgrep", Report: Report{SrcOnly: 100, TwoHop: 30}},
	})
	drifts := cur.Drift(base, 0.05)
	if len(drifts) != 1 || drifts[0].Repo != "ripgrep" {
		t.Fatalf("drifts = %+v, want only ripgrep", drifts)
	}
}
