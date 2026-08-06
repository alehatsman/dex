package store

import (
	"context"
	"testing"
	"time"
)

func pathByID(t *testing.T, ctx context.Context, st *Store, id int64) string {
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
	if got := pathByID(t, ctx, st, hits[0].id); got != "internal/auth_handler.go" {
		t.Fatalf("path-token query matched the wrong chunk: %s", got)
	}
}

// NOTE: the former TestFTSExternalContentDriftHealed (#432) is gone. It
// exercised the in-place migrateChunkContext rebuild that healed a drifted
// external-content index. Under the versioned schema (#431) that drift is
// structurally impossible: the chunks_* triggers are built once from a single
// document expression (chunkFTSContentExpr), and any index written by an older,
// buggy binary fails the schema_version gate in migrate() and is rebuilt by
// `dex reindex` — there is no in-place heal path left to test.
