package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

func TestExportKnowledgeRawSurvivesSchemaMismatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "x.db")

	// Build a store, add notes, close.
	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	for _, b := range []string{"alpha note", "beta note", "gamma note"} {
		if _, err := st.KnowledgeAdd(ctx, "Gotcha", b, 0.8); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	// Corrupt the stored schema_version so the migrate-gated Open fail-closes —
	// exactly the state a future schema bump (the notes-evolution cluster) puts
	// an existing index in.
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE meta SET value='99999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	// The normal open must now fail (proves we're in the mismatch state).
	if _, err := Open(ctx, dbPath); err == nil {
		t.Fatal("expected store.Open to fail on the corrupted schema_version")
	}

	// The raw export must STILL rescue every note.
	got, err := ExportKnowledgeRaw(ctx, dbPath)
	if err != nil {
		t.Fatalf("ExportKnowledgeRaw: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rescued %d notes across the schema mismatch, want 3", len(got))
	}
	bodies := map[string]bool{}
	for _, b := range got {
		bodies[b.Body] = true
		if b.Archetype != "Gotcha" || b.Confidence <= 0 {
			t.Errorf("garbled note: %+v", b)
		}
	}
	for _, want := range []string{"alpha note", "beta note", "gamma note"} {
		if !bodies[want] {
			t.Errorf("missing rescued note %q", want)
		}
	}
}

// TestReindexRestorePreservesScopeAndCounters locks #678: the reindex rescue
// path (ExportKnowledgeRaw → KnowledgeRestore) must round-trip the scope binding
// and the salience signal (created_at / hit_count / revision_count). Before the
// fix the backup carried only {archetype, body, confidence} and restore re-added
// via the unscoped KnowledgeAdd, so the first reindex silently unscoped every
// scoped note and zeroed its counters.
func TestReindexRestorePreservesScopeAndCounters(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")

	st, err := Open(ctx, srcPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	const body = "the mcp server bootstraps its index lazily"
	const scope = "internal/mcp/*.go"
	if _, err := st.KnowledgeAddScoped(ctx, "Gotcha", body, 0.9, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KnowledgeAdd(ctx, "Fact", "an unscoped fact", 0.7); err != nil {
		t.Fatal(err)
	}
	// Stamp known counters/timestamps so the round-trip assertion is exact.
	const created = int64(1_700_000_000_000_000_000)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE knowledge_facts SET created_at=?, updated_at=?, hit_count=5, revision_count=3 WHERE body=?`,
		created, created+1000, body); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// The raw export (reindex rescue read) must carry scope + counters now.
	backup, err := ExportKnowledgeRaw(ctx, srcPath)
	if err != nil {
		t.Fatalf("ExportKnowledgeRaw: %v", err)
	}
	var scoped *KnowledgeBackup
	for i := range backup {
		if backup[i].Body == body {
			scoped = &backup[i]
		}
	}
	if scoped == nil {
		t.Fatal("scoped note missing from raw export")
	}
	if scoped.Scope != scope {
		t.Errorf("export dropped scope: got %q want %q", scoped.Scope, scope)
	}
	if scoped.HitCount != 5 || scoped.RevisionCount != 3 {
		t.Errorf("export dropped counters: hit=%d rev=%d, want 5/3", scoped.HitCount, scoped.RevisionCount)
	}
	if scoped.CreatedAt != created {
		t.Errorf("export dropped created_at: got %d want %d", scoped.CreatedAt, created)
	}

	// Restore into a fresh store, exactly as the reindex does.
	st2, err := Open(ctx, filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	for _, b := range backup {
		if err := st2.KnowledgeRestore(ctx, b); err != nil {
			t.Fatalf("KnowledgeRestore: %v", err)
		}
	}

	// The scope binding must survive: touching a file under internal/mcp surfaces it.
	hits, err := st2.KnowledgeByScope(ctx, "internal/mcp/server.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		switch h.Body {
		case body:
			found = true
			if h.Scope != scope {
				t.Errorf("restored scope = %q, want %q", h.Scope, scope)
			}
			if h.HitCount != 5 || h.RevisionCount != 3 {
				t.Errorf("restored counters hit=%d rev=%d, want 5/3 (salience signal reset)", h.HitCount, h.RevisionCount)
			}
			if h.CreatedAt.UnixNano() != created {
				t.Errorf("restored created_at = %d, want %d", h.CreatedAt.UnixNano(), created)
			}
		case "an unscoped fact":
			t.Error("unscoped note leaked into KnowledgeByScope after restore")
		}
	}
	if !found {
		t.Error("scoped note did NOT survive the reindex restore — KnowledgeByScope no longer matches it (regression #678)")
	}
}

func TestExportKnowledgeRawMissingFile(t *testing.T) {
	got, err := ExportKnowledgeRaw(context.Background(), filepath.Join(t.TempDir(), "nope.db"))
	if err != nil || got != nil {
		t.Errorf("missing file should yield (nil, nil), got (%v, %v)", got, err)
	}
}
