package store

import (
	"testing"
	"time"
)

func vec4(a, b, c, d float32) []float32 { return []float32{a, b, c, d} }

// TestCloneClusters: two near-identical function blocks in different files form
// one cluster; an orthogonal block is left out.
func TestCloneClusters(t *testing.T) {
	st, ctx := newStore(t)
	rows := []PendingChunk{
		{Path: "a/x.go", Kind: "function", Name: "Serialize", StartLine: 1, EndLine: 20, ContentSHA: "h1",
			Content: "serialize config to xml", Vec: vec4(1, 0, 0, 0)},
		{Path: "b/y.go", Kind: "function", Name: "SerializeXML", StartLine: 1, EndLine: 20, ContentSHA: "h2",
			Content: "serialize config to xml other", Vec: vec4(0.999, 0.001, 0, 0)},
		{Path: "c/z.go", Kind: "function", Name: "Handler", StartLine: 1, EndLine: 20, ContentSHA: "h3",
			Content: "http route handler dispatch", Vec: vec4(0, 1, 0, 0)},
	}
	if err := st.UpsertMany(ctx, rows, time.Now()); err != nil {
		t.Fatal(err)
	}

	clusters, err := st.CloneClusters(ctx, CloneOpts{Threshold: 0.9, MinLines: 6, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 clone cluster, got %d: %+v", len(clusters), clusters)
	}
	if len(clusters[0].Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(clusters[0].Members))
	}
	if clusters[0].Similarity < 0.9 {
		t.Errorf("similarity floor %v below threshold", clusters[0].Similarity)
	}
	for _, m := range clusters[0].Members {
		if m.Path == "c/z.go" {
			t.Errorf("orthogonal block wrongly clustered")
		}
	}
}

// TestCloneClustersMinLines: near-identical but too-short blocks are filtered.
func TestCloneClustersMinLines(t *testing.T) {
	st, ctx := newStore(t)
	rows := []PendingChunk{
		{Path: "a/x.go", Kind: "function", StartLine: 1, EndLine: 3, ContentSHA: "h1", Content: "x", Vec: vec4(1, 0, 0, 0)},
		{Path: "b/y.go", Kind: "function", StartLine: 1, EndLine: 3, ContentSHA: "h2", Content: "y", Vec: vec4(1, 0, 0, 0)},
	}
	if err := st.UpsertMany(ctx, rows, time.Now()); err != nil {
		t.Fatal(err)
	}
	clusters, err := st.CloneClusters(ctx, CloneOpts{Threshold: 0.9, MinLines: 6, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Errorf("short blocks should be filtered by min_lines, got %d clusters", len(clusters))
	}
}

// TestCloneClustersKindFilter: non-code chunk kinds are ignored even if identical.
func TestCloneClustersKindFilter(t *testing.T) {
	st, ctx := newStore(t)
	rows := []PendingChunk{
		{Path: "a/x.go", Kind: "window", StartLine: 1, EndLine: 20, ContentSHA: "h1", Content: "x", Vec: vec4(1, 0, 0, 0)},
		{Path: "b/y.go", Kind: "window", StartLine: 1, EndLine: 20, ContentSHA: "h2", Content: "y", Vec: vec4(1, 0, 0, 0)},
	}
	if err := st.UpsertMany(ctx, rows, time.Now()); err != nil {
		t.Fatal(err)
	}
	clusters, err := st.CloneClusters(ctx, CloneOpts{Threshold: 0.9, MinLines: 6, K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Errorf("non-function kinds should be ignored, got %d clusters", len(clusters))
	}
}
