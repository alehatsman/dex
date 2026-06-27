package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/retrieve"
)

// envelopeCeilingBytes must leave headroom above the inline pool for the lanes
// that sit outside it (graph, knowledge_facts, answer).
func TestEnvelopeCeilingExceedsInlinePool(t *testing.T) {
	for _, intent := range []string{retrieve.IntentArchitecture, retrieve.IntentBehaviorSearch} {
		pool := retrieve.InlineCapsFor(intent).TotalBytesCap
		if got := envelopeCeilingBytes(intent); got <= pool {
			t.Errorf("envelopeCeilingBytes(%s)=%d, want > pool cap %d", intent, got, pool)
		}
	}
}

// A bundle already under budget is returned untouched.
func TestClampEnvelopeNoOpUnderBudget(t *testing.T) {
	out := ContextOutput{
		NextAction:   "read foo.go",
		Graph:        &GraphResult{Nodes: []GraphNode{{ID: "a"}}, Edges: []GraphEdge{{From: "a", To: "b"}}},
		SemanticHits: []SemHit{{Path: "foo.go", Content: "small"}},
	}
	clampResponseEnvelope(&out, retrieve.IntentBehaviorSearch)
	if out.Graph == nil || len(out.Graph.Edges) != 1 {
		t.Error("graph trimmed despite under-budget bundle")
	}
	if out.SemanticHits[0].Content != "small" {
		t.Error("content trimmed despite under-budget bundle")
	}
	if out.NextAction != "read foo.go" {
		t.Errorf("next_action mutated: %q", out.NextAction)
	}
}

// The graph is shed before any inlined code when the bundle is over budget.
func TestClampEnvelopeDropsGraphFirst(t *testing.T) {
	out := ContextOutput{
		NextAction:   "orient",
		SemanticHits: []SemHit{{Path: "keep.go", Content: strings.Repeat("x", 2000)}},
	}
	// A graph large enough on its own to blow the targeted ceiling.
	edges := make([]GraphEdge, 0, 1200)
	nodes := make([]GraphNode, 0, 1200)
	for i := 0; i < 1200; i++ {
		id := fmt.Sprintf("internal/pkg/longish_symbol_name_%05d", i)
		nodes = append(nodes, GraphNode{ID: id, QualifiedName: id, Kind: "func"})
		edges = append(edges, GraphEdge{From: id, To: id, Kind: "calls"})
	}
	out.Graph = &GraphResult{Nodes: nodes, Edges: edges}

	ceiling := envelopeCeilingBytes(retrieve.IntentBehaviorSearch)
	if envelopeSizeBytes(&out) <= ceiling {
		t.Fatal("test setup: bundle not over budget before clamp")
	}
	clampResponseEnvelope(&out, retrieve.IntentBehaviorSearch)

	if sz := envelopeSizeBytes(&out); sz > ceiling {
		t.Errorf("after clamp size=%d still over ceiling=%d", sz, ceiling)
	}
	if out.Graph != nil && len(out.Graph.Edges) > 0 {
		t.Error("graph edges survived an over-budget clamp")
	}
	if out.SemanticHits[0].Content == "" {
		t.Error("inlined code was shed before the graph")
	}
	if !strings.Contains(out.NextAction, "trimmed to fit") {
		t.Errorf("no trim notice in next_action: %q", out.NextAction)
	}
}

// When the graph is gone and content alone still overflows, the lowest-ranked
// inlined hits are shed tail-first while the top hit is preserved.
func TestClampEnvelopeShedsContentTailKeepingTopHit(t *testing.T) {
	ceiling := envelopeCeilingBytes(retrieve.IntentBehaviorSearch)
	hits := make([]SemHit, 10)
	for i := range hits {
		hits[i] = SemHit{Path: fmt.Sprintf("f%d.go", i), Content: strings.Repeat("y", 5000)}
	}
	out := ContextOutput{SemanticHits: hits} // ~50 KB of content, no graph

	if envelopeSizeBytes(&out) <= ceiling {
		t.Fatal("test setup: bundle not over budget before clamp")
	}
	clampResponseEnvelope(&out, retrieve.IntentBehaviorSearch)

	if sz := envelopeSizeBytes(&out); sz > ceiling {
		t.Errorf("after clamp size=%d still over ceiling=%d", sz, ceiling)
	}
	if out.SemanticHits[0].Content == "" {
		t.Error("top hit content was shed")
	}
	shed := 0
	for _, h := range out.SemanticHits[1:] {
		if h.Content == "" {
			if !h.Truncated {
				t.Error("shed hit not flagged Truncated")
			}
			shed++
		}
	}
	if shed == 0 {
		t.Error("no tail content shed despite overflow")
	}
}
