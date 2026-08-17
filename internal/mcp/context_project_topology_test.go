package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/retrieve"
)

// TestFromPackGraphCarriesRanking locks the #190 wire contract: the
// package_topology lane's in/out-degree + PageRank ride onto GraphNode, and a
// plain node (no centrality) serializes at the old lean {id,kind} shape because
// the fields are omitempty.
func TestFromPackGraphCarriesRanking(t *testing.T) {
	in := &retrieve.GraphResult{
		Nodes: []retrieve.GraphNode{
			{ID: "store", QualifiedName: "mod/internal/store", Kind: "package", InDegree: 12, OutDegree: 2, PageRank: 0.031},
			{ID: "leaf", Kind: "package"}, // no centrality — a non-topology node
		},
	}
	out := fromPackGraph(in)
	if out == nil || len(out.Nodes) != 2 {
		t.Fatalf("fromPackGraph = %+v, want 2 nodes", out)
	}

	if got := out.Nodes[0]; got.InDegree != 12 || got.OutDegree != 2 || got.PageRank != 0.031 {
		t.Errorf("ranking not carried: %+v", got)
	}

	// Ranked node serializes with the metrics.
	ranked, _ := json.Marshal(out.Nodes[0])
	for _, key := range []string{`"in_degree":12`, `"out_degree":2`, `"page_rank":0.031`} {
		if !strings.Contains(string(ranked), key) {
			t.Errorf("ranked node JSON missing %s: %s", key, ranked)
		}
	}

	// Plain node stays lean — omitempty drops all three.
	plain, _ := json.Marshal(out.Nodes[1])
	for _, key := range []string{"in_degree", "out_degree", "page_rank"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("plain node leaked %s (should be omitempty): %s", key, plain)
		}
	}
}
