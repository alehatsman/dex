package store

import (
	"context"
	"testing"
	"time"
)

// ftsMatchCount counts rows in the FTS index matching a raw MATCH expression.
// It hits chunks_fts directly (no JOIN to chunks), so it sees orphaned
// postings whose content row has been deleted — exactly the #432 drift symptom.
func ftsMatchCount(t *testing.T, st *Store, ctx context.Context, match string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(ctx,
		`SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, match).Scan(&n); err != nil {
		t.Fatalf("fts match count %q: %v", match, err)
	}
	return n
}

func pathByID(t *testing.T, st *Store, ctx context.Context, id int64) string {
	t.Helper()
	var p string
	if err := st.db.QueryRowContext(ctx,
		`SELECT path FROM chunks WHERE id=?`, id).Scan(&p); err != nil {
		t.Fatalf("path for id %d: %v", id, err)
	}
	return p
}

// TestFTSPathColumnCoversPathTokens pins the assumption behind closing #433
// wont-fix: chunks_fts has a dedicated `path` column whose unicode61 tokenizer
// already splits "internal/auth_handler.go" into internal/auth/handler/go, and
// scoreBM25 searches it unqualified at weight 2.0. A term present ONLY in a
// chunk's path — never in its body — must still surface that chunk, which is
// why folding the same tokens into `content` is redundant.
func TestFTSPathColumnCoversPathTokens(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()
	rows := []PendingChunk{
		{Path: "internal/auth_handler.go", Kind: "fn", ContentSHA: "h1", Content: "func validate() {}", Vec: []float32{1, 0}},
		{Path: "other.go", Kind: "fn", ContentSHA: "h2", Content: "func validate() {}", Vec: []float32{0, 1}},
	}
	if err := st.UpsertMany(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	// "handler" occurs only in the first chunk's path, in neither body.
	hits, err := st.scoreBM25(ctx, "handler", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly the path-matching chunk for 'handler'; got %d hits", len(hits))
	}
	if got := pathByID(t, st, ctx, hits[0].id); got != "internal/auth_handler.go" {
		t.Fatalf("path-token query matched the wrong chunk: %s", got)
	}
}

// TestFTSExternalContentDriftHealed reproduces #432 and verifies the migration
// rebuild heals it. chunks_fts is an external-content table, so the document
// stored in the index must stay in lock-step with what the triggers emit. The
// buggy first cut swapped the triggers to emit context||content but did NOT
// rebuild, leaving every existing row indexed under fts_path_enrich's
// content+path-tokens document. The result is a content column carrying path
// tokens the live triggers would never produce — drift that mis-scores BM25 and
// orphans those postings on any later edit/delete of the row.
func TestFTSExternalContentDriftHealed(t *testing.T) {
	st, ctx := newStore(t)
	now := time.Now()
	rows := []PendingChunk{
		{Path: "alphauniq.go", Kind: "fn", ContentSHA: "h1", Content: "func Aone() {}", Vec: []float32{1, 0}},
		{Path: "betauniq.go", Kind: "fn", ContentSHA: "h2", Content: "func Btwo() {}", Vec: []float32{0, 1}},
	}
	if err := st.UpsertMany(ctx, rows, now); err != nil {
		t.Fatal(err)
	}

	// Recreate the pre-v2 index state: rows indexed with the old fts_path_enrich
	// document (content + folded path tokens) while the live triggers emit the
	// newer context||content document — the exact version skew that drifts. The
	// folded path token "alphauniq" lands in the CONTENT column, where the live
	// triggers would never put it.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO chunks_fts(chunks_fts) VALUES('delete-all')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO chunks_fts(rowid, content, path, kind)
		 SELECT id,
		        content || ' ' || replace(replace(replace(path, '/', ' '), '.', ' '), '_', ' '),
		        path, kind
		   FROM chunks`); err != nil {
		t.Fatal(err)
	}
	if got := ftsMatchCount(t, st, ctx, "content:alphauniq"); got != 1 {
		t.Fatalf("precondition: expected the drifted content posting (got %d) — "+
			"if 0, the drift wasn't reproduced", got)
	}

	// Heal: re-run the migration (it short-circuits on the flag otherwise).
	if _, err := st.db.ExecContext(ctx, `DELETE FROM meta WHERE key='chunk_fts_content_v2'`); err != nil {
		t.Fatal(err)
	}
	if err := st.migrateChunkContext(ctx); err != nil {
		t.Fatalf("heal migration: %v", err)
	}

	// The rebuild re-indexed every row through context||content, so the path
	// token is gone from the content column...
	if got := ftsMatchCount(t, st, ctx, "content:alphauniq"); got != 0 {
		t.Fatalf("drifted content posting survived the rebuild: %d", got)
	}
	// ...while both chunks remain findable by their path token via the path
	// column (which always covered path queries — see #433).
	for _, tok := range []string{"alphauniq", "betauniq"} {
		hits, err := st.scoreBM25(ctx, tok, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Fatalf("path token %q unfindable after rebuild", tok)
		}
	}
}
