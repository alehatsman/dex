package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// readVecQuant reads the persisted quantization mode straight from the meta
// table via a fresh read-only connection (the store must be closed first).
// meta is a plain table, so this never touches the vec0 virtual table.
func readVecQuant(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'vec_quant'`).Scan(&v); err != nil {
		t.Fatalf("read vec_quant: %v", err)
	}
	return v
}

// TestStatusOpenerPreservesVectorQuant pins #334: CLI runtime store opens must
// go through openStore (env-aware) so a read-only path like the all-project
// `dex index status` listing does not silently rewrite an index's vector
// quantization mode. openStore threads DEX_VECTOR_QUANT via storeOpts(); the
// bare store.Open uses default Options{} → quant resolves to float32, and
// ensureVecTable then drops+rebuilds chunk_vecs on an int8 index.
func TestStatusOpenerPreservesVectorQuant(t *testing.T) {
	t.Setenv("DEX_VECTOR_QUANT", "int8")
	dbPath := filepath.Join(t.TempDir(), "q.db")
	ctx := context.Background()

	// Build an int8 index through the CLI opener and seed a vector row so the
	// store records a dim and writes meta.vec_quant.
	st, err := openStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMany(ctx, []store.PendingChunk{{
		Path: "a.go", Kind: "fn", StartLine: 1, EndLine: 2,
		ContentSHA: "h1", Content: "func A() {}",
		Vec: []float32{1, 0, 0, 0},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := readVecQuant(t, dbPath); got != "int8" {
		t.Fatalf("freshly built index vec_quant = %q, want int8", got)
	}

	// A read-only reopen through the env-aware opener must NOT requant.
	st, err = openStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := readVecQuant(t, dbPath); got != "int8" {
		t.Errorf("openStore flipped vec_quant to %q; read-only path must preserve int8", got)
	}

	// Guard the bug: the bare store.Open ignores DEX_VECTOR_QUANT and rewrites
	// the table to float32 — exactly why status/listing/clone paths must use
	// openStore. If this assertion ever fails, the requant contract changed
	// and the rationale for #334 needs revisiting.
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := readVecQuant(t, dbPath); got != "float32" {
		t.Errorf("store.Open did not exhibit the documented requant (vec_quant=%q); test assumptions are stale", got)
	}
}
