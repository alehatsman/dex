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
