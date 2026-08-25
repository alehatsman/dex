package store

import (
	"context"
	"fmt"
	"strings"

	"database/sql"
)

// sidecarVecSpec describes a simple vec0 sidecar table that mirrors a source
// row table: vectors are keyed on the source rowid and dropped by an AFTER
// DELETE trigger when the source row goes away. Unlike chunk_vecs there is no
// insert/update mirroring or quant lane — the owner writes vectors explicitly.
// Used by knowledge fact embeddings (fact_vecs).
type sidecarVecSpec struct {
	vecTable   string // vec0 table name, e.g. "fact_vecs"
	srcTable   string // row table the delete-cascade trigger fires on
	trigger    string // AFTER DELETE trigger name
	dimMetaKey string // meta key recording the current dim
}

// ensureSidecarVecTable materializes spec's vec0 virtual table at dim plus its
// delete-cascade trigger. Idempotent. If the table exists at a different
// dimension (embed model changed) it is dropped and recreated — vectors
// re-backfill lazily on the next write. dim<=0 is a no-op.
func ensureSidecarVecTable(ctx context.Context, db *sql.DB, spec sidecarVecSpec, dim int) error {
	if dim <= 0 {
		return nil
	}
	var recorded string
	_ = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, spec.dimMetaKey).Scan(&recorded)
	want := fmt.Sprintf("%d", dim)
	if recorded != "" && recorded != want {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+spec.vecTable); err != nil {
			return fmt.Errorf("ensure %s: drop on dim change: %w", spec.vecTable, err)
		}
	}
	stmts := []string{
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(
		   embedding FLOAT[%d] distance_metric=cosine
		 )`, spec.vecTable, dim),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s AFTER DELETE ON %s BEGIN
		   DELETE FROM %s WHERE rowid = old.id;
		 END`, spec.trigger, spec.srcTable, spec.vecTable),
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensure %s: %w", spec.vecTable, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		   ON CONFLICT(key) DO UPDATE SET value=excluded.value`, spec.dimMetaKey, want); err != nil {
		return fmt.Errorf("ensure %s: record dim: %w", spec.vecTable, err)
	}
	return nil
}

func (s *Store) quantMode() string {
	if strings.EqualFold(strings.TrimSpace(s.opts.VectorQuant), "int8") {
		return "int8"
	}
	return "float32"
}

// vecColumnType is the vec0 column declaration for the active quant mode.
func vecColumnType(mode string, dim int64) string {
	if mode == "int8" {
		return fmt.Sprintf("int8[%d]", dim)
	}
	return fmt.Sprintf("FLOAT[%d]", dim)
}

// vecStoreExpr wraps a float32 vec-BLOB SQL expression (`col`) for storage
// into chunk_vecs under the active mode. int8 quantizes with sqlite-vec's
// unit range ([-1,1]→int8), which fits the L2-normalized embeddings dex
// stores; float32 passes the BLOB through unchanged.
func vecStoreExpr(mode, col string) string {
	if mode == "int8" {
		return fmt.Sprintf("vec_quantize_int8(%s, 'unit')", col)
	}
	return col
}

// vecMatchExpr wraps the bound query-vector placeholder for a KNN MATCH
// under the active mode, so the query vector is quantized identically to
// the stored doc vectors (cosine is then computed int8-vs-int8 in-engine).
func (s *Store) vecMatchExpr() string {
	return vecStoreExpr(s.quantMode(), "?")
}

// ensureVecTable materializes the sqlite-vec vec0 virtual table and the
// triggers that keep it in sync with `chunks`. The dim is fixed at CREATE
// time, so this is a no-op until s.dim is known (either recovered from
// meta.dim on Open or set on the first UpsertMany).
//
// The chunk_vecs element type (FLOAT vs int8) follows the active quant
// mode. chunks.vec always holds full-precision float32 — the int8 lane is
// purely a smaller/faster KNN copy derived from it.
//
// On first creation against a pre-vec0 index (chunks already populated),
// it backfills chunk_vecs from chunks.vec in one INSERT...SELECT. Cheap
// because vec0 takes the BLOB format we already store on disk (packed
// little-endian float32).
func (s *Store) ensureVecTable(ctx context.Context, dim int64) error {
	if dim <= 0 {
		return nil
	}
	mode := s.quantMode()

	// A mode flip (operator toggled DEX_VECTOR_QUANT) leaves chunk_vecs
	// declared with the wrong element type. vec0 fixes the type at CREATE,
	// so the only fix is to drop the table + its triggers and rebuild from
	// the full-precision chunks.vec source of truth. A legacy index has no
	// vec_quant meta key — it predates quantization, so it is float32.
	var stored string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='`+metaVecQuant+`'`).Scan(&stored); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("ensure vec table: read vec_quant: %w", err)
	}
	if stored == "" {
		stored = "float32"
	}
	if stored != mode {
		for _, q := range []string{
			`DROP TRIGGER IF EXISTS chunks_vec_ai`,
			`DROP TRIGGER IF EXISTS chunks_vec_ad`,
			`DROP TRIGGER IF EXISTS chunks_vec_au`,
			`DROP TABLE IF EXISTS chunk_vecs`,
		} {
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("ensure vec table: drop for requant: %w (%s)", err, q)
			}
		}
	}

	storeExpr := vecStoreExpr(mode, "new.vec")
	stmts := []string{
		// vec0 keeps the embedding in its own storage; cosine distance
		// lets the rest of the search code keep treating "larger score =
		// better" (we return 1 - distance for callers). The element type
		// (FLOAT vs int8) is the quant knob; cosine over int8 is native.
		fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vecs USING vec0(
		   embedding %s distance_metric=cosine
		 )`, vecColumnType(mode, dim)),
		// Mirror chunks.vec into chunk_vecs.embedding. We piggyback on
		// chunks.id (== rowid) as the join key, matching the FTS5 pattern
		// already in use for chunks_fts. Under int8, the stored f32 BLOB is
		// quantized on the way in; chunks.vec itself stays full precision.
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS chunks_vec_ai AFTER INSERT ON chunks BEGIN
		   INSERT INTO chunk_vecs(rowid, embedding) VALUES (new.id, %s);
		 END`, storeExpr),
		`CREATE TRIGGER IF NOT EXISTS chunks_vec_ad AFTER DELETE ON chunks BEGIN
		   DELETE FROM chunk_vecs WHERE rowid = old.id;
		 END`,
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS chunks_vec_au AFTER UPDATE OF vec ON chunks BEGIN
		   DELETE FROM chunk_vecs WHERE rowid = new.id;
		   INSERT INTO chunk_vecs(rowid, embedding) VALUES (new.id, %s);
		 END`, storeExpr),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensure vec table: %w (%s)", err, q)
		}
	}
	// One-shot backfill for indexes that pre-date sqlite-vec. Triggers
	// only fire on future writes, so any chunks already on disk need to
	// be pushed into chunk_vecs explicitly. Cheap and idempotent: if
	// chunk_vecs is already populated, the SELECT yields zero new rows.
	var vecRows, chunkRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_vecs`).Scan(&vecRows); err != nil {
		return fmt.Errorf("ensure vec table: count chunk_vecs: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&chunkRows); err != nil {
		return fmt.Errorf("ensure vec table: count chunks: %w", err)
	}
	if vecRows == 0 && chunkRows > 0 {
		if _, err := s.db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO chunk_vecs(rowid, embedding) SELECT id, %s FROM chunks`,
				vecStoreExpr(mode, "vec"))); err != nil {
			return fmt.Errorf("ensure vec table: backfill: %w", err)
		}
	}
	// Record the active encoding so the next Open can detect a mode flip
	// and rebuild chunk_vecs from chunks.vec.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		metaVecQuant, mode); err != nil {
		return fmt.Errorf("ensure vec table: record vec_quant: %w", err)
	}
	return nil
}

// ensureNodeVecTable creates the node_vecs sqlite-vec virtual table (always
// float32) and the triggers that keep it in sync with graph_nodes.vec.
// A one-shot backfill is run when node_vecs is empty but graph_nodes has rows
// with non-empty vecs (e.g. after an upgrade).
func (s *Store) ensureNodeVecTable(ctx context.Context, dim int64) error {
	if dim == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS node_vecs USING vec0(
		   rowid INTEGER PRIMARY KEY,
		   embedding FLOAT[%d] distance_metric=cosine
		 )`, dim)); err != nil {
		return fmt.Errorf("ensure node vec table: create: %w", err)
	}
	for _, stmt := range []string{
		`CREATE TRIGGER IF NOT EXISTS graph_nodes_vec_au
		 AFTER UPDATE OF vec ON graph_nodes BEGIN
		   DELETE FROM node_vecs WHERE rowid = old.rowid;
		   INSERT INTO node_vecs(rowid, embedding)
		     SELECT new.rowid, new.vec WHERE length(new.vec) > 0;
		 END`,
		`CREATE TRIGGER IF NOT EXISTS graph_nodes_vec_ad
		 AFTER DELETE ON graph_nodes BEGIN
		   DELETE FROM node_vecs WHERE rowid = old.rowid;
		 END`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ensure node vec table: trigger: %w", err)
		}
	}
	// One-shot backfill for indexes built before this code shipped.
	var vecRows, nodeRows int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_vecs`).Scan(&vecRows); err != nil {
		return fmt.Errorf("ensure node vec table: count node_vecs: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM graph_nodes WHERE length(vec) > 0`).Scan(&nodeRows); err != nil {
		return fmt.Errorf("ensure node vec table: count graph_nodes: %w", err)
	}
	if vecRows == 0 && nodeRows > 0 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO node_vecs(rowid, embedding) SELECT rowid, vec FROM graph_nodes WHERE length(vec) > 0`); err != nil {
			return fmt.Errorf("ensure node vec table: backfill: %w", err)
		}
	}
	return nil
}

// Stats reports the current state of an index.
