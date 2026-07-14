package graphrefresh

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// fakeEmbedder returns one deterministic fixed-dim vector per input,
// satisfying embed.Embedder without any network. Vectors differ by input
// length so they aren't all identical.
type fakeEmbedder struct{ dim int }

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		v := make([]float32, f.dim)
		v[len(s)%f.dim] = 1
		out[i] = v
	}
	return out, nil
}
func (f *fakeEmbedder) Health(context.Context) error { return nil }
func (f *fakeEmbedder) Endpoint() string             { return "fake" }
func (f *fakeEmbedder) ModelName() string            { return "fake" }
func (f *fakeEmbedder) BatchSize() int               { return 16 }

// TestEmbedNodes guards the shared graph-node embed loop that both the CLI
// index/watch paths and the MCP auto-watcher (#327) now drive: every node
// missing a vector gets one, and afterward none remain un-embedded.
func TestEmbedNodes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	if err := st.GraphUpsertNodes(ctx, []store.GraphNodeRow{
		{ID: "n:a", Kind: "function", Name: "A", QualifiedName: "pkg.A", FilePath: "a.go", ContentHash: "ha"},
		{ID: "n:b", Kind: "function", Name: "B", QualifiedName: "pkg.B", FilePath: "b.go", ContentHash: "hb"},
	}, now); err != nil {
		t.Fatal(err)
	}

	// Both nodes start un-embedded.
	pending, err := st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 nodes needing embed, got %d", len(pending))
	}

	n, err := EmbedNodes(ctx, st, &fakeEmbedder{dim: 4}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("EmbedNodes embedded %d nodes, want 2", n)
	}

	// After embedding, nothing is left needing a vector.
	left, err := st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d nodes still need embed after EmbedNodes, want 0", len(left))
	}

	// Idempotent: a second pass embeds nothing.
	n2, err := EmbedNodes(ctx, st, &fakeEmbedder{dim: 4}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("second EmbedNodes pass embedded %d nodes, want 0", n2)
	}
}
