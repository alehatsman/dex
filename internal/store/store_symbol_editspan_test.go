package store

import (
	"testing"
	"time"
)

// editSpanNode is a function node carrying the #591 refactor-target columns,
// so the round-trip and resolver tests exercise real values rather than the
// zero defaults funcNode leaves them at.
func editSpanNode(id, name, pkg, file, sig string, startByte, endByte int) GraphNodeRow {
	return GraphNodeRow{
		ID: id, Kind: "function", Name: name, QualifiedName: name,
		PackagePath: pkg, FilePath: file,
		StartLine: 1, EndLine: 10, ContentHash: id + "-h",
		Signature: sig, StartByte: startByte, EndByte: endByte,
		DeclarationHash: id + "-dh",
	}
}

// TestGraphAllNodesRoundTripsEditSpanFields locks that the four columns
// survive an upsert → GraphAllNodes round-trip (INSERT, SELECT, and Scan all
// carry them). A regression here means a column was added to one statement
// but not the others.
func TestGraphAllNodesRoundTripsEditSpanFields(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	want := editSpanNode("n1", "Run", "pkg/a", "pkg/a/a.go",
		"func (s *Server) Run(ctx context.Context) error", 42, 137)
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{want}, now); err != nil {
		t.Fatal(err)
	}

	nodes, err := st.GraphAllNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes=%d, want 1", len(nodes))
	}
	got := nodes[0]
	if got.Signature != want.Signature {
		t.Errorf("Signature=%q, want %q", got.Signature, want.Signature)
	}
	if got.StartByte != want.StartByte || got.EndByte != want.EndByte {
		t.Errorf("byte span=(%d,%d), want (%d,%d)", got.StartByte, got.EndByte, want.StartByte, want.EndByte)
	}
	if got.DeclarationHash != want.DeclarationHash {
		t.Errorf("DeclarationHash=%q, want %q", got.DeclarationHash, want.DeclarationHash)
	}
}

// TestGraphUpsertRefreshesEditSpanFields verifies the ON CONFLICT branch
// updates the new columns: re-upserting the same ID with a changed signature
// and byte span overwrites the stored values rather than keeping the originals.
func TestGraphUpsertRefreshesEditSpanFields(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	first := editSpanNode("n1", "Run", "pkg/a", "pkg/a/a.go", "func Run()", 10, 20)
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{first}, now); err != nil {
		t.Fatal(err)
	}
	second := editSpanNode("n1", "Run", "pkg/a", "pkg/a/a.go", "func Run(ctx context.Context)", 10, 35)
	second.ContentHash = "n1-h2" // content changed → row must refresh
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{second}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	span, ok, err := st.SymbolEditSpanByID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("SymbolEditSpanByID: ok=false, want true")
	}
	if span.Signature != second.Signature {
		t.Errorf("Signature=%q, want refreshed %q", span.Signature, second.Signature)
	}
	if span.EndByte != 35 {
		t.Errorf("EndByte=%d, want refreshed 35", span.EndByte)
	}
}

// TestSymbolEditSpanByID covers the precise resolver: a hit returns the full
// span, a miss returns ok=false with a nil error (not a sql.ErrNoRows leak).
func TestSymbolEditSpanByID(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	n := editSpanNode("n1", "Parse", "pkg/p", "pkg/p/p.go", "func Parse(b []byte) (T, error)", 100, 260)
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{n}, now); err != nil {
		t.Fatal(err)
	}

	span, ok, err := st.SymbolEditSpanByID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if span.ID != "n1" || span.FilePath != "pkg/p/p.go" || span.Kind != "function" {
		t.Errorf("span identity = %+v, want id=n1 file=pkg/p/p.go kind=function", span)
	}
	if span.StartByte != 100 || span.EndByte != 260 {
		t.Errorf("byte span=(%d,%d), want (100,260)", span.StartByte, span.EndByte)
	}

	_, ok, err = st.SymbolEditSpanByID(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("miss should not error: %v", err)
	}
	if ok {
		t.Error("ok=true for a missing id, want false")
	}
}

// TestSymbolEditSpansByName covers the ambiguous-name resolver: a name defined
// in two packages returns both spans, ordered by file path then start line.
func TestSymbolEditSpansByName(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	a := editSpanNode("na", "New", "pkg/a", "pkg/a/a.go", "func New() *A", 5, 40)
	b := editSpanNode("nb", "New", "pkg/b", "pkg/b/b.go", "func New() *B", 7, 55)
	if err := st.GraphUpsertNodes(ctx, []GraphNodeRow{b, a}, now); err != nil {
		t.Fatal(err)
	}

	spans, err := st.SymbolEditSpansByName(ctx, "New")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("spans=%d, want 2", len(spans))
	}
	// Ordered by file_path: pkg/a/a.go before pkg/b/b.go.
	if spans[0].FilePath != "pkg/a/a.go" || spans[1].FilePath != "pkg/b/b.go" {
		t.Errorf("order=[%s,%s], want [pkg/a/a.go,pkg/b/b.go]", spans[0].FilePath, spans[1].FilePath)
	}
	if spans[0].Signature != "func New() *A" {
		t.Errorf("spans[0].Signature=%q, want %q", spans[0].Signature, "func New() *A")
	}

	none, err := st.SymbolEditSpansByName(ctx, "Nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("spans for missing name=%d, want 0", len(none))
	}
}
