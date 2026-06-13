package store

import (
	"context"
	"fmt"
	"strings"
)

// migrate runs the ordered list of idempotent schema statements. Its
// cyclomatic complexity comes from the flat sequence of CREATE/ALTER
// migrations, not from branching logic — splitting it would scatter the
// schema across helpers without reducing real complexity, so cyclop is
// suppressed here (mirrors the index.Run / main dispatch exclusions).
//
//nolint:cyclop // sequential schema migrations; extraction adds indirection, not clarity
func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
		   key   TEXT PRIMARY KEY,
		   value TEXT NOT NULL
		 )`,
		`CREATE TABLE IF NOT EXISTS chunks (
		   id            INTEGER PRIMARY KEY,
		   path          TEXT NOT NULL,
		   kind          TEXT NOT NULL,
		   name          TEXT NOT NULL DEFAULT '',
		   start_line    INTEGER NOT NULL,
		   end_line      INTEGER NOT NULL,
		   content_sha1  TEXT NOT NULL,
		   content       TEXT NOT NULL,
		   vec           BLOB NOT NULL,
		   last_seen_at  INTEGER NOT NULL,
		   UNIQUE(path, content_sha1)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_path ON chunks(path)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_last_seen ON chunks(last_seen_at)`,
		// FTS5 external-content index. Doesn't duplicate chunk text on
		// disk — it references chunks.content by rowid=chunks.id and
		// only persists tokenizer state. We keep it in sync via AFTER
		// triggers on chunks. Hybrid Search fuses cosine ranking with
		// BM25 ranking over this index via RRF.
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
		   content, path, kind,
		   content='chunks', content_rowid='id',
		   tokenize='unicode61 remove_diacritics 2'
		 )`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id, new.content, new.path, new.kind);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES('delete', old.id, old.content, old.path, old.kind);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES('delete', old.id, old.content, old.path, old.kind);
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id, new.content, new.path, new.kind);
		 END`,
		// graph_nodes / graph_edges hold the structural index produced by
		// the graph phase of `dex index`. The schema is intentionally string-keyed
		// (id TEXT) so node identities are stable across re-extraction and
		// independent of SQLite's rowid. chunk_id links back to chunks.id
		// for callers that want code text plus structural neighborhood;
		// no FK constraint — chunks can be re-upserted with new rowids and
		// we re-resolve the linkage on the next graph index pass.
		`CREATE TABLE IF NOT EXISTS graph_nodes (
		   id              TEXT PRIMARY KEY,
		   kind            TEXT NOT NULL,
		   name            TEXT NOT NULL,
		   qualified_name  TEXT NOT NULL,
		   package_path    TEXT NOT NULL DEFAULT '',
		   file_path       TEXT NOT NULL DEFAULT '',
		   start_line      INTEGER NOT NULL DEFAULT 0,
		   end_line        INTEGER NOT NULL DEFAULT 0,
		   chunk_id        INTEGER,
		   metadata_json   TEXT NOT NULL DEFAULT '{}',
		   content_hash    TEXT NOT NULL,
		   last_seen_at    INTEGER NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_graph_nodes_kind      ON graph_nodes(kind)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_nodes_name      ON graph_nodes(name)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_nodes_package   ON graph_nodes(package_path)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_nodes_file      ON graph_nodes(file_path)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_nodes_last_seen ON graph_nodes(last_seen_at)`,
		`CREATE TABLE IF NOT EXISTS graph_edges (
		   id              TEXT PRIMARY KEY,
		   kind            TEXT NOT NULL,
		   src_id          TEXT NOT NULL,
		   dst_id          TEXT NOT NULL,
		   file_path       TEXT NOT NULL DEFAULT '',
		   start_line      INTEGER NOT NULL DEFAULT 0,
		   end_line        INTEGER NOT NULL DEFAULT 0,
		   metadata_json   TEXT NOT NULL DEFAULT '{}',
		   content_hash    TEXT NOT NULL,
		   last_seen_at    INTEGER NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_graph_edges_src       ON graph_edges(src_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_edges_dst       ON graph_edges(dst_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_graph_edges_last_seen ON graph_edges(last_seen_at)`,
		// sessions / session_files — lightweight cross-call memory: current
		// task, notes, and recently-accessed files. One session per project
		// (highest id = current). Files cascade-delete with the session.
		`CREATE TABLE IF NOT EXISTS sessions (
		   id           INTEGER PRIMARY KEY AUTOINCREMENT,
		   started_at   INTEGER NOT NULL,
		   updated_at   INTEGER NOT NULL,
		   task         TEXT NOT NULL DEFAULT '',
		   notes        TEXT NOT NULL DEFAULT ''
		 )`,
		`CREATE TABLE IF NOT EXISTS session_files (
		   id         INTEGER PRIMARY KEY AUTOINCREMENT,
		   session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		   path       TEXT NOT NULL,
		   op         TEXT NOT NULL DEFAULT 'read',
		   touched_at INTEGER NOT NULL,
		   UNIQUE(session_id, path)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_session_files_session ON session_files(session_id, touched_at)`,
		// knowledge_facts — agent-accumulated project facts. One row per
		// unique body (UNIQUE constraint). Salience computed on read.
		`CREATE TABLE IF NOT EXISTS knowledge_facts (
		   id         INTEGER PRIMARY KEY AUTOINCREMENT,
		   archetype  TEXT NOT NULL DEFAULT 'Observation',
		   body       TEXT NOT NULL,
		   confidence REAL NOT NULL DEFAULT 0.8,
		   created_at INTEGER NOT NULL,
		   updated_at INTEGER NOT NULL,
		   hit_count      INTEGER NOT NULL DEFAULT 0,
		   revision_count INTEGER NOT NULL DEFAULT 0,
		   UNIQUE(body)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_knowledge_confidence ON knowledge_facts(confidence DESC, updated_at DESC)`,
		// agents / agent_messages — multi-agent coordination bus. Agents
		// announce themselves, post findings, and read peers' messages.
		// Useful when multiple concurrent agents share one dex instance.
		`CREATE TABLE IF NOT EXISTS agents (
		   id           TEXT PRIMARY KEY,
		   role         TEXT NOT NULL DEFAULT '',
		   announced_at INTEGER NOT NULL,
		   last_seen_at INTEGER NOT NULL
		 )`,
		`CREATE TABLE IF NOT EXISTS agent_messages (
		   id        INTEGER PRIMARY KEY AUTOINCREMENT,
		   agent_id  TEXT NOT NULL,
		   topic     TEXT NOT NULL DEFAULT '',
		   body      TEXT NOT NULL,
		   posted_at INTEGER NOT NULL
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_agent_messages_topic ON agent_messages(topic, id)`,
		// share_cache — shared compressed-file-context cache for parallel agents.
		// Keyed by path (one entry per file). Evicted on hash mismatch (pull).
		`CREATE TABLE IF NOT EXISTS share_cache (
		   id           INTEGER PRIMARY KEY AUTOINCREMENT,
		   path         TEXT NOT NULL,
		   content_hash TEXT NOT NULL,
		   content      TEXT NOT NULL,
		   pushed_by    TEXT NOT NULL DEFAULT '',
		   pushed_at    INTEGER NOT NULL,
		   hit_count    INTEGER NOT NULL DEFAULT 0,
		   UNIQUE(path)
		 )`,
		// ctx_packages — registry of installed context packages (.ctxpkg bundles).
		`CREATE TABLE IF NOT EXISTS ctx_packages (
		   name       TEXT PRIMARY KEY,
		   created_at INTEGER NOT NULL,
		   auto_load  INTEGER NOT NULL DEFAULT 0
		 )`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w (%s)", err, q)
		}
	}
	// Backfill chunks_fts for databases that pre-date the hybrid search
	// migration. Cheap on first-run (one INSERT-from-SELECT batch);
	// guarded by a meta flag so we don't pay it on every Open.
	var built string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='fts_built'`).Scan(&built)
	if built != "1" {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO chunks_fts(chunks_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("migrate: fts rebuild: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('fts_built', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: fts flag: %w", err)
		}
	}
	// Add name column to existing databases that pre-date this migration.
	// On fresh databases the column already exists from CREATE TABLE, so
	// we silently ignore "duplicate column name" errors from ALTER TABLE.
	var nameColAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='name_col_added'`).Scan(&nameColAdded)
	if nameColAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE chunks ADD COLUMN name TEXT NOT NULL DEFAULT ''`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add name column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('name_col_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: name_col flag: %w", err)
		}
	}
	// Centrality columns on graph_nodes — populated post-extract from
	// the `calls` edges. All four default to 0 so untouched rows behave
	// as "unknown" (sort_by_centrality just deprioritises them). Added
	// idempotently per the name_col pattern.
	var centralityColsAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='centrality_cols_added'`).Scan(&centralityColsAdded)
	if centralityColsAdded != "1" {
		alters := []string{
			`ALTER TABLE graph_nodes ADD COLUMN in_degree INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE graph_nodes ADD COLUMN out_degree INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE graph_nodes ADD COLUMN cross_pkg_callers INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE graph_nodes ADD COLUMN pagerank REAL NOT NULL DEFAULT 0`,
		}
		for _, q := range alters {
			if _, err := s.db.ExecContext(ctx, q); err != nil &&
				!strings.Contains(err.Error(), "duplicate column name") {
				return fmt.Errorf("migrate: %s: %w", q, err)
			}
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('centrality_cols_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: centrality flag: %w", err)
		}
	}
	var betweennessColAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='betweenness_col_added'`).Scan(&betweennessColAdded)
	if betweennessColAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE graph_nodes ADD COLUMN betweenness REAL NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add betweenness column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('betweenness_col_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: betweenness flag: %w", err)
		}
	}
	var communityColAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='community_id_col_added'`).Scan(&communityColAdded)
	if communityColAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE graph_nodes ADD COLUMN community_id INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add community_id column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('community_id_col_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: community_id flag: %w", err)
		}
	}
	var knowledgeRevColAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='knowledge_rev_col_added'`).Scan(&knowledgeRevColAdded)
	if knowledgeRevColAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE knowledge_facts ADD COLUMN revision_count INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add revision_count column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('knowledge_rev_col_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: knowledge_rev_col flag: %w", err)
		}
	}
	// last_retrieved (#225): tracks when a fact was last surfaced (distinct
	// from updated_at = last confirmed). Drives decay protection so
	// frequently-recalled facts fade slower. Defaults to 0 (never retrieved).
	var knowledgeLastRetrievedAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='knowledge_last_retrieved_added'`).Scan(&knowledgeLastRetrievedAdded)
	if knowledgeLastRetrievedAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE knowledge_facts ADD COLUMN last_retrieved INTEGER NOT NULL DEFAULT 0`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add last_retrieved column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('knowledge_last_retrieved_added', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: knowledge_last_retrieved flag: %w", err)
		}
	}
	// Path enrichment (#110): rebuild FTS triggers so that each chunk's
	// BM25 document includes path component tokens (split on '/', '.', '_').
	// Enables queries like "auth handler" to surface "auth_handler.go"
	// even when content alone wouldn't rank it first.
	// Requires a full FTS rebuild because existing rows were indexed
	// without the path suffix.
	var ftsPathEnrich string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='fts_path_enrich'`).Scan(&ftsPathEnrich)
	if ftsPathEnrich != "1" {
		pathEnrichMigration := []string{
			// Drop the old triggers that inserted plain content.
			`DROP TRIGGER IF EXISTS chunks_ai`,
			`DROP TRIGGER IF EXISTS chunks_ad`,
			`DROP TRIGGER IF EXISTS chunks_au`,
			// New triggers: append path component tokens to the FTS content
			// field. replace(replace(replace(path,'/','.'),' ','_',' '))
			// produces "internal store auth handler go" from
			// "internal/store/auth_handler.go".
			`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
			   INSERT INTO chunks_fts(rowid, content, path, kind)
			   VALUES (new.id,
			           new.content || ' ' || replace(replace(replace(new.path, '/', ' '), '.', ' '), '_', ' '),
			           new.path, new.kind);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
			   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
			   VALUES('delete', old.id,
			          old.content || ' ' || replace(replace(replace(old.path, '/', ' '), '.', ' '), '_', ' '),
			          old.path, old.kind);
			 END`,
			`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
			   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
			   VALUES('delete', old.id,
			          old.content || ' ' || replace(replace(replace(old.path, '/', ' '), '.', ' '), '_', ' '),
			          old.path, old.kind);
			   INSERT INTO chunks_fts(rowid, content, path, kind)
			   VALUES (new.id,
			           new.content || ' ' || replace(replace(replace(new.path, '/', ' '), '.', ' '), '_', ' '),
			           new.path, new.kind);
			 END`,
		}
		for _, q := range pathEnrichMigration {
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("migrate fts_path_enrich: %w (%s)", err, q)
			}
		}
		// Rebuild FTS from scratch: drop + recreate so existing rows get
		// the enriched content. The 'delete-all' + INSERT-from-SELECT
		// pattern avoids dropping the FTS virtual table itself (which would
		// require recreating shadow tables and re-registering the tokenizer).
		if _, err := s.db.ExecContext(ctx, `INSERT INTO chunks_fts(chunks_fts) VALUES('delete-all')`); err != nil {
			return fmt.Errorf("migrate fts_path_enrich: delete-all: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO chunks_fts(rowid, content, path, kind)
			 SELECT id,
			        content || ' ' || replace(replace(replace(path, '/', ' '), '.', ' '), '_', ' '),
			        path, kind
			 FROM chunks`); err != nil {
			return fmt.Errorf("migrate fts_path_enrich: repopulate: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('fts_path_enrich', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate fts_path_enrich: flag: %w", err)
		}
	}
	// ContextBus (#148): add category column to agent_messages + FTS5 index.
	// category groups messages by semantic kind (e.g. "finding", "plan", "error").
	var agentMsgCatAdded string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='agent_msg_category'`).Scan(&agentMsgCatAdded)
	if agentMsgCatAdded != "1" {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE agent_messages ADD COLUMN category TEXT NOT NULL DEFAULT ''`); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate: add agent_messages.category: %w", err)
		}
		// FTS5 external-content table for full-text search over messages.
		if _, err := s.db.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS agent_messages_fts USING fts5(
			body, topic, category,
			content='agent_messages', content_rowid='id'
		)`); err != nil {
			return fmt.Errorf("migrate: agent_messages_fts create: %w", err)
		}
		// Triggers to keep FTS in sync.
		for _, trig := range []string{
			`CREATE TRIGGER IF NOT EXISTS agent_messages_ai AFTER INSERT ON agent_messages BEGIN
			   INSERT INTO agent_messages_fts(rowid, body, topic, category)
			   VALUES (new.id, new.body, new.topic, new.category); END`,
			`CREATE TRIGGER IF NOT EXISTS agent_messages_ad AFTER DELETE ON agent_messages BEGIN
			   INSERT INTO agent_messages_fts(agent_messages_fts, rowid, body, topic, category)
			   VALUES ('delete', old.id, old.body, old.topic, old.category); END`,
			`CREATE TRIGGER IF NOT EXISTS agent_messages_au AFTER UPDATE ON agent_messages BEGIN
			   INSERT INTO agent_messages_fts(agent_messages_fts, rowid, body, topic, category)
			   VALUES ('delete', old.id, old.body, old.topic, old.category);
			   INSERT INTO agent_messages_fts(rowid, body, topic, category)
			   VALUES (new.id, new.body, new.topic, new.category); END`,
		} {
			if _, err := s.db.ExecContext(ctx, trig); err != nil {
				return fmt.Errorf("migrate: agent_messages trigger: %w", err)
			}
		}
		// Backfill existing rows into FTS (no-op on fresh databases).
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO agent_messages_fts(rowid, body, topic, category)
			 SELECT id, body, topic, COALESCE(category, '') FROM agent_messages`); err != nil &&
			!strings.Contains(err.Error(), "UNIQUE constraint") {
			return fmt.Errorf("migrate: agent_messages_fts backfill: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('agent_msg_category', '1')
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("migrate: agent_msg_category flag: %w", err)
		}
	}
	if err := s.migrateCoAccessEdges(ctx); err != nil {
		return err
	}
	// Each migration is invoked independently from migrate() and self-guards
	// on its own meta flag. Never chain one migration inside another's
	// "not done yet" branch: a DB that already cleared the outer flag would
	// skip the inner migration forever.
	if err := s.migrateChunkContext(ctx); err != nil {
		return err
	}
	return s.migrateGraphNodeVec(ctx)
}

func (s *Store) migrateCoAccessEdges(ctx context.Context) error {
	var done string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='co_access_edges_added'`).Scan(&done)
	if done == "1" {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS co_access_edges (
			src_path      TEXT NOT NULL,
			dst_path      TEXT NOT NULL,
			weight        REAL NOT NULL DEFAULT 1.0,
			reinforced_at INTEGER NOT NULL,
			PRIMARY KEY (src_path, dst_path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_coaccess_src ON co_access_edges(src_path)`,
		`CREATE INDEX IF NOT EXISTS idx_coaccess_dst ON co_access_edges(dst_path)`,
		`INSERT INTO meta(key, value) VALUES('co_access_edges_added', '1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrateCoAccessEdges: %w", err)
		}
	}
	return nil
}

// migrateGraphNodeVec adds vec/vec_hash columns to graph_nodes for symbol KNN.
func (s *Store) migrateGraphNodeVec(ctx context.Context) error {
	for _, col := range []struct{ name, def string }{
		{"vec", "BLOB NOT NULL DEFAULT X''"},
		{"vec_hash", "TEXT NOT NULL DEFAULT ''"},
	} {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE graph_nodes ADD COLUMN `+col.name+` `+col.def); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrateGraphNodeVec: add %s: %w", col.name, err)
		}
	}
	return nil
}

// chunkFTSContentExpr builds the SQL expression for a chunk's FTS `content`
// document: the Contextual-BM25 prefix (context_text + newline, when present)
// followed by the chunk body.
//
// Path tokens are deliberately NOT folded in here. chunks_fts has a dedicated
// `path` column whose unicode61 tokenizer already splits "internal/auth_handler.go"
// into "internal","auth","handler","go", and scoreBM25 searches it unqualified
// at weight 2.0 — so path query terms are already covered. Folding the same
// tokens into `content` (as an earlier cut did) only double-counts them and
// skews BM25; see #433 (closed wont-fix).
//
// Because chunks_fts is an external-content table, every site that writes to
// it — the INSERT/DELETE/UPDATE triggers and the rebuild's INSERT…SELECT —
// must emit the byte-identical document, or a DELETE won't match what was
// indexed and the FTS index silently drifts (#432). Generating all of them
// from this one helper guarantees that. `ref` is the row alias ("new"/"old"
// inside a trigger) or "" for a bare-column SELECT.
func chunkFTSContentExpr(ref string) string {
	col := func(name string) string {
		if ref == "" {
			return name
		}
		return ref + "." + name
	}
	context, content := col("context_text"), col("content")
	return fmt.Sprintf(
		`CASE WHEN %[1]s IS NOT NULL AND %[1]s != '' `+
			`THEN %[1]s || CHAR(10) || %[2]s ELSE %[2]s END`,
		context, content)
}

// migrateChunkContext adds context_text to chunks and (re)builds the FTS5
// triggers so each chunk's BM25 document is the Contextual-BM25 text (see
// chunkFTSContentExpr).
//
// This runs under the chunk_fts_content_v2 flag rather than the original
// chunk_context_added so it also heals indexes built by the buggy first cut,
// which swapped in the context_text triggers WITHOUT rebuilding the index.
// Those indexes still hold the prior fts_path_enrich documents (content +
// folded path tokens), so a later DELETE — whose trigger now emits only
// context||content — fails to remove the path-token postings, orphaning them:
// external-content drift (#432). The rebuild below re-indexes every existing
// row through the current expression, healing the drift and dropping the now
// redundant in-content path tokens (the `path` column still covers them, #433).
//
// The rebuild is a delete-all + INSERT…SELECT with the document expression,
// NOT the FTS5 'rebuild' command: 'rebuild' re-reads the raw chunks columns
// and would index plain content, discarding the context_text prefix the
// triggers apply and re-introducing a mismatch.
func (s *Store) migrateChunkContext(ctx context.Context) error {
	var done string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='chunk_fts_content_v2'`).Scan(&done)
	if done == "1" {
		return nil
	}
	// Add context_text column (no-op on duplicate).
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE chunks ADD COLUMN context_text TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrateChunkContext: add context_text: %w", err)
	}
	// Recreate FTS5 triggers. DROP first because CREATE TRIGGER IF NOT EXISTS
	// won't replace an existing (possibly older-shape) trigger.
	for _, drop := range []string{
		`DROP TRIGGER IF EXISTS chunks_ai`,
		`DROP TRIGGER IF EXISTS chunks_ad`,
		`DROP TRIGGER IF EXISTS chunks_au`,
	} {
		if _, err := s.db.ExecContext(ctx, drop); err != nil {
			return fmt.Errorf("migrateChunkContext: drop trigger: %w", err)
		}
	}
	newDoc, oldDoc := chunkFTSContentExpr("new"), chunkFTSContentExpr("old")
	for _, trig := range []string{
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id, %s, new.path, new.kind);
		 END`, newDoc),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES ('delete', old.id, %s, old.path, old.kind);
		 END`, oldDoc),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES ('delete', old.id, %s, old.path, old.kind);
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id, %s, new.path, new.kind);
		 END`, oldDoc, newDoc),
	} {
		if _, err := s.db.ExecContext(ctx, trig); err != nil {
			return fmt.Errorf("migrateChunkContext: create trigger: %w", err)
		}
	}
	// Rebuild the FTS index so existing rows carry the enriched document and
	// match what the DELETE triggers now emit. delete-all + repopulate (see
	// the doc comment for why not 'rebuild').
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO chunks_fts(chunks_fts) VALUES('delete-all')`); err != nil {
		return fmt.Errorf("migrateChunkContext: delete-all: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO chunks_fts(rowid, content, path, kind)
		 SELECT id, %s, path, kind FROM chunks`, chunkFTSContentExpr(""))); err != nil {
		return fmt.Errorf("migrateChunkContext: repopulate: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('chunk_fts_content_v2', '1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return fmt.Errorf("migrateChunkContext: flag: %w", err)
	}
	return nil
}

// quantMode returns the canonical chunk_vecs encoding for this store:
// "int8" when DEX_VECTOR_QUANT selects scalar quantization, "float32"
// otherwise. This canonical string is recorded in meta.vec_quant and
// compared on Open to detect a mode flip that needs a rebuild.
