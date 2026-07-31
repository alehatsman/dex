package store

import (
	"testing"
	"time"
)

// TestNodesNeedingEmbedGate verifies the #91 re-embed gate keys on the embed
// text (kind + qualified name), not content_hash: a pure line/byte shift (new
// content_hash, identical embed text) must NOT re-embed, while a rename must.
func TestNodesNeedingEmbedGate(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	upsert := func(name, qname, contentHash string) {
		t.Helper()
		if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{
			{ID: "n:a", Kind: "function", Name: name, QualifiedName: qname,
				FilePath: "a.go", ContentHash: contentHash},
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	// New node → needs embed; embed it.
	upsert("A", "pkg.A", "h1")
	rows, err := st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("new node: got %d needing embed, want 1", len(rows))
	}
	if err := st.SetNodeVecs(ctx, rows, [][]float32{{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// Line shift: same embed text, new content_hash → must NOT re-embed.
	upsert("A", "pkg.A", "h2")
	rows, err = st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("after line shift: got %d needing embed, want 0 (identical embed text)", len(rows))
	}

	// Rename: qualified name changes → embed text changes → must re-embed.
	upsert("A2", "pkg.A2", "h3")
	rows, err = st.NodesNeedingEmbed(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EmbedText != "function pkg.A2" {
		t.Fatalf("after rename: got %d rows (%v), want 1 with embed text %q",
			len(rows), rows, "function pkg.A2")
	}
}

// TestNodeEmbedTextSQLMirrorsGo guards nodeEmbedTextSQL against drift from the
// Go nodeEmbedText — the two MUST agree byte-for-byte or the gate mis-fires.
func TestNodeEmbedTextSQLMirrorsGo(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	cases := []GraphNodeRow{
		{ID: "q1", Kind: "function", Name: "A", QualifiedName: "pkg.A", FilePath: "a.go", ContentHash: "1"},
		{ID: "q2", Kind: "method", Name: "M", QualifiedName: "pkg.T.M", FilePath: "b.go", ContentHash: "2"},
		// Empty qualified name → nodeEmbedText falls back to the bare name.
		{ID: "q3", Kind: "type", Name: "Bare", QualifiedName: "", FilePath: "c.go", ContentHash: "3"},
	}
	if err := st.GraphUpsertNodes(ctx, cases, now); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		var got string
		if err := st.db.QueryRowContext(ctx,
			`SELECT `+nodeEmbedTextSQL+` FROM graph_nodes WHERE id=?`, c.ID).Scan(&got); err != nil {
			t.Fatalf("node %s: %v", c.ID, err)
		}
		if want := nodeEmbedText(c.Kind, c.Name, c.QualifiedName); got != want {
			t.Errorf("node %s: SQL embed text %q != Go %q", c.ID, got, want)
		}
	}
}
