package store

import (
	"testing"
	"time"
)

func TestExternalImports(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	rows := []GraphNodeRow{
		// A project package — its path makes an import to it "internal".
		{ID: "p1", Kind: "package", Name: "x", QualifiedName: "x", PackagePath: "example.com/proj/internal/x"},
		// Internal import (matches the project package path) → excluded.
		{ID: "i1", Kind: "import", Name: "x", QualifiedName: "example.com/proj/internal/x"},
		// External third-party + stdlib imports → included, deduped, sorted.
		{ID: "i2", Kind: "import", Name: "go-sqlite3", QualifiedName: "github.com/mattn/go-sqlite3"},
		{ID: "i3", Kind: "import", Name: "http", QualifiedName: "net/http"},
		{ID: "i4", Kind: "import", Name: "http", QualifiedName: "net/http"}, // dup
	}
	if err := st.GraphUpsertNodes(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	ext, err := st.ExternalImports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"github.com/mattn/go-sqlite3", "net/http"}
	if len(ext) != len(want) {
		t.Fatalf("ExternalImports = %v, want %v", ext, want)
	}
	for i := range want {
		if ext[i] != want[i] {
			t.Errorf("ExternalImports[%d] = %q, want %q (sorted, deduped, no internal)", i, ext[i], want[i])
		}
	}
}

func TestMainEntrypoints(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	rows := []GraphNodeRow{
		{ID: "m1", Kind: "function", Name: "main", QualifiedName: "main", PackagePath: "example.com/proj/cmd/a", FilePath: "cmd/a/main.go"},
		{ID: "m2", Kind: "function", Name: "main", QualifiedName: "main", PackagePath: "example.com/proj/cmd/b", FilePath: "cmd/b/main.go"},
		// Not a main → excluded.
		{ID: "f1", Kind: "function", Name: "helper", QualifiedName: "helper", PackagePath: "example.com/proj/cmd/a", FilePath: "cmd/a/main.go"},
		// A "main"-named method (not a function) → excluded.
		{ID: "x1", Kind: "method", Name: "main", QualifiedName: "(*T).main", PackagePath: "example.com/proj/internal/x", FilePath: "internal/x/x.go"},
		// Test-fixture mains under testdata/ → excluded as noise.
		{ID: "t1", Kind: "function", Name: "main", QualifiedName: "main", PackagePath: "p", FilePath: "internal/graph/testdata/rust_simple/src/main.rs"},
		{ID: "t2", Kind: "function", Name: "main", QualifiedName: "main", PackagePath: "p", FilePath: "vendor/foo/main.go"},
	}
	if err := st.GraphUpsertNodes(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	eps, err := st.MainEntrypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd/a/main.go", "cmd/b/main.go"}
	if len(eps) != len(want) {
		t.Fatalf("MainEntrypoints = %v, want %v", eps, want)
	}
	for i := range want {
		if eps[i] != want[i] {
			t.Errorf("MainEntrypoints[%d] = %q, want %q (sorted, only main functions)", i, eps[i], want[i])
		}
	}
}

func TestGraphScale(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()

	nodes := []GraphNodeRow{
		// 2 project packages.
		{ID: "p1", Kind: "package", Name: "x", QualifiedName: "x", PackagePath: "example.com/proj/x"},
		{ID: "p2", Kind: "package", Name: "y", QualifiedName: "y", PackagePath: "example.com/proj/y"},
		// 2 source files.
		{ID: "fl1", Kind: "file", Name: "x.go", FilePath: "x/x.go"},
		{ID: "fl2", Kind: "file", Name: "y.go", FilePath: "y/y.go"},
		// 4 declarations across the declarable kinds.
		{ID: "fn1", Kind: "function", Name: "A", QualifiedName: "x.A", FilePath: "x/x.go"},
		{ID: "me1", Kind: "method", Name: "M", QualifiedName: "(*T).M", FilePath: "x/x.go"},
		{ID: "st1", Kind: "struct", Name: "T", QualifiedName: "x.T", FilePath: "x/x.go"},
		{ID: "if1", Kind: "interface", Name: "I", QualifiedName: "y.I", FilePath: "y/y.go"},
		// Non-declaration kinds → excluded from the symbol count.
		{ID: "fld", Kind: "field", Name: "f", FilePath: "x/x.go"},
		{ID: "imp", Kind: "import", Name: "fmt", QualifiedName: "fmt"},
		// Fixtures → excluded everywhere.
		{ID: "td1", Kind: "file", Name: "m.rs", FilePath: "internal/graph/testdata/r/main.rs"},
		{ID: "td2", Kind: "function", Name: "fixture", QualifiedName: "fixture", FilePath: "internal/graph/testdata/r/main.rs"},
		{ID: "vd1", Kind: "struct", Name: "V", QualifiedName: "v.V", FilePath: "vendor/v/v.go"},
	}
	if err := st.GraphUpsertNodes(ctx, nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []GraphEdgeRow{
		{ID: "e1", Kind: "calls", SrcID: "fn1", DstID: "me1"},
		{ID: "e2", Kind: "calls", SrcID: "me1", DstID: "fn1"},
		{ID: "e3", Kind: "contains", SrcID: "st1", DstID: "me1"}, // not a call → excluded
	}
	if err := st.GraphUpsertEdges(ctx, edges, now); err != nil {
		t.Fatal(err)
	}

	g, err := st.GraphScale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Files != 2 {
		t.Errorf("Files = %d, want 2 (testdata excluded)", g.Files)
	}
	if g.Packages != 2 {
		t.Errorf("Packages = %d, want 2", g.Packages)
	}
	if g.Symbols != 4 {
		t.Errorf("Symbols = %d, want 4 (func+method+struct+interface; field/import/testdata/vendor excluded)", g.Symbols)
	}
	if g.CallEdges != 2 {
		t.Errorf("CallEdges = %d, want 2 (only 'calls' edges)", g.CallEdges)
	}
	if g.Empty() {
		t.Error("a populated graph should not report Empty")
	}
}

func TestGraphScaleEmpty(t *testing.T) {
	st, ctx := newStore(t)
	g, err := st.GraphScale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Empty() {
		t.Errorf("empty index should report Empty; got %+v", g)
	}
}
