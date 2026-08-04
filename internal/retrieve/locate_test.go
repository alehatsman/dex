package retrieve

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantLine int
		wantOK   bool
	}{
		{"internal/mcp/server.go:313", "internal/mcp/server.go", 313, true},
		{"foo.go:42:7", "foo.go", 42, true}, // path:line:col
		{"  foo.go:1  ", "foo.go", 1, true},
		{"noline.go", "", 0, false},
		{"foo.go:notanum", "", 0, false},
	}
	for _, c := range cases {
		p, l, ok := parseRef(c.in)
		if ok != c.wantOK || p != c.wantPath || l != c.wantLine {
			t.Errorf("parseRef(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.in, p, l, ok, c.wantPath, c.wantLine, c.wantOK)
		}
	}
}

func TestParseFrame(t *testing.T) {
	cases := []struct {
		in         string
		wantRef    string
		wantSymbol string
	}{
		{"\tinternal/foo.go:42 +0x1a5", "internal/foo.go:42", ""},
		{"panic at server.go:10", "server.go:10", ""},
		{"github.com/x/mcp.(*Server).Run(0xc1)", "", "(*Server).Run"},
		{"github.com/x/mcp.NewServer(0x0)", "", "mcp.NewServer"},
		{"Greet", "", "Greet"},
	}
	for _, c := range cases {
		ref, sym := parseFrame(c.in)
		if ref != c.wantRef || sym != c.wantSymbol {
			t.Errorf("parseFrame(%q) = (ref=%q, sym=%q), want (ref=%q, sym=%q)",
				c.in, ref, sym, c.wantRef, c.wantSymbol)
		}
	}
}

// TestResolveBySymbolPrefersMethodForReceiverQualified guards #702: when the
// symbol input is receiver-qualified (*T).X, resolveBySymbol must prefer
// method/function graph nodes over field nodes even when the field has higher
// pagerank. Without the fix the field would win because it's sorted first.
func TestResolveBySymbolPrefersMethodForReceiverQualified(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	defer st.Close()

	now := time.Now()
	// Insert two graph nodes with the same name "Do": a field (high pagerank)
	// and a method (low pagerank). No chunk rows, so FindSymbol falls through
	// to graph_nodes and the field would win by pagerank without the fix.
	nodes := []store.GraphNodeRow{
		{
			ID: "field:Config.Do", Kind: "field", Name: "Do",
			QualifiedName: "Config.Do", PackagePath: "pkg/a",
			FilePath: "a/config.go", StartLine: 5, EndLine: 5, ContentHash: "h1",
		},
		{
			ID: "method:B.Do", Kind: "method", Name: "Do",
			QualifiedName: "(*B).Do", PackagePath: "pkg/b",
			FilePath: "b/b.go", StartLine: 3, EndLine: 6, ContentHash: "h2",
		},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	// Give the field higher pagerank so it wins the default sort.
	if err := st.GraphSetCentrality(ctx, []store.GraphCentralityRow{
		{ID: "field:Config.Do", InDegree: 10, PageRank: 0.5},
		{ID: "method:B.Do", InDegree: 1, PageRank: 0.01},
	}); err != nil {
		t.Fatal(err)
	}

	// Receiver-qualified input must prefer the method despite lower pagerank.
	got := resolveBySymbol(ctx, st, "(*B).Do")
	if got.Status != "ok" {
		t.Fatalf("status=%q hint=%q", got.Status, got.Hint)
	}
	if got.Kind != "method" && got.Kind != "function" {
		t.Errorf("kind=%q for (*B).Do, want method or function (field won instead)", got.Kind)
	}

	// Bare input (no receiver) must be unaffected — field still wins by pagerank.
	gotBare := resolveBySymbol(ctx, st, "Do")
	if gotBare.Status != "ok" {
		t.Fatalf("bare status=%q hint=%q", gotBare.Status, gotBare.Hint)
	}
	if gotBare.Kind != "field" {
		t.Errorf("bare kind=%q for Do, want field (pagerank-ranked result)", gotBare.Kind)
	}
}
