package graphquery

import (
	"testing"

	"github.com/alehatsman/dex/internal/graph"
)

// impactView builds a small call graph:
//
//	store.Search  <-- mcp.contextRouter  <-- mcp.RunStdio
//	store.Search  <-- index.Run          (separate caller, same depth)
//
// PageRank: contextRouter > index.Run (to verify sort order at depth 1).
func impactView() (*View, Node) {
	fn := func(pkg, name, file string, line int, pr float64) Node {
		qn := pkg + "." + name
		return Node{
			ID:            graph.NodeID("", pkg, graph.NodeFunction, qn),
			Kind:          graph.NodeFunction,
			Name:          name,
			QualifiedName: qn,
			PackagePath:   pkg,
			FilePath:      file,
			StartLine:     line,
			EndLine:       line + 10,
			PageRank:      pr,
		}
	}

	seed := fn("store", "Search", "internal/store/store.go", 100, 0.9)
	caller1 := fn("mcp", "contextRouter", "internal/mcp/context.go", 50, 0.7)
	caller2 := fn("index", "Run", "internal/index/index.go", 30, 0.3)
	depth2 := fn("mcp", "RunStdio", "internal/mcp/server.go", 10, 0.5)

	callEdge := func(src, dst Node) Edge {
		return Edge{Kind: graph.EdgeCalls, SrcID: src.ID, DstID: dst.ID,
			FilePath: src.FilePath, StartLine: src.StartLine}
	}
	edges := []Edge{
		callEdge(caller1, seed),
		callEdge(caller2, seed),
		callEdge(depth2, caller1),
	}

	v := &View{
		NodesByID:        map[string]Node{},
		NodesByName:      map[string][]Node{},
		NodesByQualified: map[string][]Node{},
		NodesByPath:      map[string][]Node{},
		EdgesBySrc:       map[string][]Edge{},
		EdgesByDst:       map[string][]Edge{},
		EdgesByKind:      map[graph.EdgeKind][]Edge{},
	}
	for _, n := range []Node{seed, caller1, caller2, depth2} {
		v.NodesByID[n.ID] = n
		v.NodesByName[n.Name] = append(v.NodesByName[n.Name], n)
		v.NodesByQualified[n.QualifiedName] = append(v.NodesByQualified[n.QualifiedName], n)
		v.NodesByPath[n.FilePath] = append(v.NodesByPath[n.FilePath], n)
	}
	for _, e := range edges {
		v.EdgesBySrc[e.SrcID] = append(v.EdgesBySrc[e.SrcID], e)
		v.EdgesByDst[e.DstID] = append(v.EdgesByDst[e.DstID], e)
		v.EdgesByKind[e.Kind] = append(v.EdgesByKind[e.Kind], e)
	}
	return v, seed
}

func TestComputeImpactNodes(t *testing.T) {
	view, seed := impactView()

	nodes := ComputeImpact(view, []Node{seed}, 3)

	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %v", len(nodes), nodes)
	}

	// depth-1 nodes come first, sorted by PageRank desc
	if nodes[0].Depth != 1 || nodes[0].QualifiedName != "mcp.contextRouter" {
		t.Errorf("nodes[0] want depth=1 mcp.contextRouter, got depth=%d %s", nodes[0].Depth, nodes[0].QualifiedName)
	}
	if nodes[1].Depth != 1 || nodes[1].QualifiedName != "index.Run" {
		t.Errorf("nodes[1] want depth=1 index.Run, got depth=%d %s", nodes[1].Depth, nodes[1].QualifiedName)
	}
	// depth-2 node
	if nodes[2].Depth != 2 || nodes[2].QualifiedName != "mcp.RunStdio" {
		t.Errorf("nodes[2] want depth=2 mcp.RunStdio, got depth=%d %s", nodes[2].Depth, nodes[2].QualifiedName)
	}
}

func TestComputeImpactNodesDepthCap(t *testing.T) {
	view, seed := impactView()

	nodes := ComputeImpact(view, []Node{seed}, 1)

	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes at depth 1, got %d", len(nodes))
	}
	for _, n := range nodes {
		if n.Depth != 1 {
			t.Errorf("expected depth=1, got %d for %s", n.Depth, n.QualifiedName)
		}
	}
}

func TestComputeImpactNodesSeedNotIncluded(t *testing.T) {
	view, seed := impactView()
	nodes := ComputeImpact(view, []Node{seed}, 3)
	for _, n := range nodes {
		if n.QualifiedName == seed.QualifiedName {
			t.Errorf("seed %s must not appear in impact output", seed.QualifiedName)
		}
	}
}
