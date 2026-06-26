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
