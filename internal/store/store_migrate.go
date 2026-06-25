package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// schemaVersion is the on-disk schema this binary builds and understands.
// Bump it whenever schemaDDL changes shape. An index recorded with a
// different value is rejected on Open (see migrate) — dex treats the index
// as a disposable derived artifact, so the recovery path is a one-time
// `dex reindex`, never an in-place upgrade. This replaces the old accretive
// flag-guarded ALTER chain (#431): the schema is built once, correctly, and
// version-gated rather than patched forward on every Open.
// v3 (#591): graph_nodes gains the four refactor-target columns
// (signature, start_byte, end_byte, declaration_hash). Empty byte spans are
// useless to a refactor consumer, so the upgrade is a reindex (the
// fail-closed gate below), not an ALTER-with-defaults backfill.
const schemaVersion = "3"

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
// it must emit the byte-identical document, or a DELETE won't match what was
// indexed and the FTS index silently drifts (#432). The three chunks_*
// triggers are the only write sites; generating all of them from this one
// helper guarantees byte-identity. `ref` is the row alias ("new"/"old") inside
// a trigger.
func chunkFTSContentExpr(ref string) string {
	context, content := ref+".context_text", ref+".content"
	return fmt.Sprintf(
		`CASE WHEN %[1]s IS NOT NULL AND %[1]s != '' `+
			`THEN %[1]s || CHAR(10) || %[2]s ELSE %[2]s END`,
		context, content)
}

// schemaDDL is the canonical, final-state schema. Every statement is
// idempotent (CREATE ... IF NOT EXISTS) so a fresh build and a re-assert of
// the current version are both safe. There is intentionally no ALTER/backfill
// path: an older index fails the version gate and is rebuilt from source.
func schemaDDL() []string {
	newDoc, oldDoc := chunkFTSContentExpr("new"), chunkFTSContentExpr("old")
	return []string{
		`CREATE TABLE IF NOT EXISTS meta (
		   key   TEXT PRIMARY KEY,
		   value TEXT NOT NULL
		 )`,
		// chunks — the canonical text+vector store. context_text holds the
		// optional Contextual-BM25 prefix; it is folded into the FTS document by
		// the chunks_* triggers below (nullable: most chunks have none).
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
		   context_text  TEXT,
		   UNIQUE(path, content_sha1)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_path ON chunks(path)`,
		`CREATE INDEX IF NOT EXISTS idx_chunks_last_seen ON chunks(last_seen_at)`,
		// FTS5 external-content index. Doesn't duplicate chunk text on disk — it
		// references chunks.content by rowid=chunks.id and only persists tokenizer
		// state. Kept in sync via the AFTER triggers below. Hybrid Search fuses
		// cosine ranking with BM25 ranking over this index via RRF. The dedicated
		// `path` column makes path components searchable on their own (#433).
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
		   content, path, kind,
		   content='chunks', content_rowid='id',
		   tokenize='unicode61 remove_diacritics 2'
		 )`,
		// chunks_* triggers keep the external-content FTS index in sync. Each
		// emits the chunk's BM25 document via chunkFTSContentExpr so the
		// INSERT/DELETE/UPDATE sites stay byte-identical (#432).
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
		// graph_nodes / graph_edges hold the structural index produced by the
		// graph phase of `dex index`. The schema is intentionally string-keyed
		// (id TEXT) so node identities are stable across re-extraction and
		// independent of SQLite's rowid. chunk_id links back to chunks.id for
		// callers that want code text plus structural neighborhood; no FK
		// constraint — chunks can be re-upserted with new rowids and we re-resolve
		// the linkage on the next graph index pass. The centrality columns
		// (in/out_degree, cross_pkg_callers, pagerank, betweenness, community_id)
		// are populated post-extract from the `calls` edges; vec/vec_hash carry the
		// symbol KNN embedding. All default to 0/empty so untouched rows are inert.
		`CREATE TABLE IF NOT EXISTS graph_nodes (
		   id                 TEXT PRIMARY KEY,
		   kind               TEXT NOT NULL,
		   name               TEXT NOT NULL,
		   qualified_name     TEXT NOT NULL,
		   package_path       TEXT NOT NULL DEFAULT '',
		   file_path          TEXT NOT NULL DEFAULT '',
		   start_line         INTEGER NOT NULL DEFAULT 0,
		   end_line           INTEGER NOT NULL DEFAULT 0,
		   chunk_id           INTEGER,
		   metadata_json      TEXT NOT NULL DEFAULT '{}',
		   content_hash       TEXT NOT NULL,
		   last_seen_at       INTEGER NOT NULL,
		   -- Refactor-target columns (#591). signature is the declaration
		   -- header (Go: gofmt-printed, body stripped); start_byte/end_byte
		   -- are the symbol's exact byte span in the source file (0-based,
		   -- end exclusive) for slice-precise edits without reading the file;
		   -- declaration_hash is a hash of the signature, distinct from the
		   -- positional content_hash, so a consumer can detect signature
		   -- drift independently of line shifts. All default empty/0 so nodes
		   -- from extractors that don't populate them stay inert.
		   signature          TEXT NOT NULL DEFAULT '',
		   start_byte         INTEGER NOT NULL DEFAULT 0,
		   end_byte           INTEGER NOT NULL DEFAULT 0,
		   declaration_hash   TEXT NOT NULL DEFAULT '',
		   in_degree          INTEGER NOT NULL DEFAULT 0,
		   out_degree         INTEGER NOT NULL DEFAULT 0,
		   cross_pkg_callers  INTEGER NOT NULL DEFAULT 0,
		   pagerank           REAL NOT NULL DEFAULT 0,
		   betweenness        REAL NOT NULL DEFAULT 0,
		   community_id       INTEGER NOT NULL DEFAULT 0,
		   vec                BLOB NOT NULL DEFAULT X'',
		   vec_hash           TEXT NOT NULL DEFAULT ''
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
		// sessions / session_files — lightweight cross-call memory: current task,
		// notes, and recently-accessed files. One session per project (highest id =
		// current). Files cascade-delete with the session.
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
		// knowledge_facts — agent-accumulated project facts. One row per unique
		// body (UNIQUE constraint). Salience computed on read; last_retrieved (#225)
		// tracks last surfacing (distinct from updated_at = last confirmed) to slow
		// decay on frequently-recalled facts.
		`CREATE TABLE IF NOT EXISTS knowledge_facts (
		   id             INTEGER PRIMARY KEY AUTOINCREMENT,
		   archetype      TEXT NOT NULL DEFAULT 'Observation',
		   body           TEXT NOT NULL,
		   confidence     REAL NOT NULL DEFAULT 0.8,
		   created_at     INTEGER NOT NULL,
		   updated_at     INTEGER NOT NULL,
		   hit_count      INTEGER NOT NULL DEFAULT 0,
		   revision_count INTEGER NOT NULL DEFAULT 0,
		   last_retrieved INTEGER NOT NULL DEFAULT 0,
		   UNIQUE(body)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_knowledge_confidence ON knowledge_facts(confidence DESC, updated_at DESC)`,
		// agents / agent_messages — multi-agent coordination bus. Agents announce
		// themselves, post findings, and read peers' messages. category groups
		// messages by semantic kind (e.g. "finding", "plan", "error"); the FTS5
		// index gives full-text search over the bus.
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
		   posted_at INTEGER NOT NULL,
		   category  TEXT NOT NULL DEFAULT ''
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_agent_messages_topic ON agent_messages(topic, id)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS agent_messages_fts USING fts5(
		   body, topic, category,
		   content='agent_messages', content_rowid='id'
		 )`,
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
		// co_access_edges — file co-access graph reinforced by session reads;
		// drives "files often touched together" suggestions.
		`CREATE TABLE IF NOT EXISTS co_access_edges (
		   src_path      TEXT NOT NULL,
		   dst_path      TEXT NOT NULL,
		   weight        REAL NOT NULL DEFAULT 1.0,
		   reinforced_at INTEGER NOT NULL,
		   PRIMARY KEY (src_path, dst_path)
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_coaccess_src ON co_access_edges(src_path)`,
		`CREATE INDEX IF NOT EXISTS idx_coaccess_dst ON co_access_edges(dst_path)`,
		// file_summaries — LLM-generated per-file prose, produced on demand by
		// `dex summarize` (#572). Deliberately ISOLATED from retrieval: no FTS
		// trigger, no vector, and no search/fusion path reads it. source_hash is
		// the file's chunk content_sha1 set (body-change signal, not the
		// positional graph_nodes.content_hash); prompt_version forces regen when
		// the summarizer prompt changes. Dropped+rebuilt with the rest of the
		// index on reindex via the schemaVersion gate.
		`CREATE TABLE IF NOT EXISTS file_summaries (
		   path           TEXT PRIMARY KEY,
		   source_hash    TEXT NOT NULL,
		   prompt_version INTEGER NOT NULL,
		   model          TEXT NOT NULL,
		   summary        TEXT NOT NULL,
		   generated_at   INTEGER NOT NULL
		 )`,
	}
}

// migrate brings the index up to the schema this binary builds. It is the
// single schema authority: a versioned, fail-closed gate, not an accretive
// in-place upgrade chain (#431).
//
//   - Fresh DB (no meta table yet): build schemaDDL and stamp schemaVersion.
//   - Same version: re-assert schemaDDL (idempotent no-op) — self-heals a
//     partially-built file.
//   - Different/absent version on an initialized DB: fail closed with
//     ErrSchemaVersionMismatch. The caller (or operator) recovers with
//     `dex reindex`, which wipes and rebuilds the index from source. Mirrors
//     the EnsureEmbedModel / vec_quant fail-closed contracts.
func (s *Store) migrate(ctx context.Context) error {
	// The meta table is created by every dex build, so its presence marks an
	// already-initialized index. A brand-new file has no tables at all.
	var dummy int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&dummy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh DB — fall through to build below.
	case err != nil:
		return fmt.Errorf("migrate: probe meta: %w", err)
	default:
		// Initialized index: enforce the version gate before touching it.
		var stored string
		_ = s.db.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key='`+metaSchemaVersion+`'`).Scan(&stored)
		if stored != schemaVersion {
			return fmt.Errorf("%w: index built with schema %q, this binary expects %q — run `dex reindex` to rebuild",
				ErrSchemaVersionMismatch, stored, schemaVersion)
		}
	}

	for _, q := range schemaDDL() {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w (%s)", err, q)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('`+metaSchemaVersion+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersion); err != nil {
		return fmt.Errorf("migrate: stamp schema_version: %w", err)
	}
	return nil
}
