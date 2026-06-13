// Package store persists per-project chunks + embedding vectors.
//
// One SQLite file per project. Vectors live in a sqlite-vec `vec0`
// virtual table (`chunk_vecs`) and KNN runs natively in SQL — cosine
// distance with a serialized float32 BLOB query. The chunks table
// still holds the raw vec BLOB so chunk_vecs can be rebuilt and so
// vec_distance_cosine() can score BM25-only hits cheaply.
//
// Timestamps (last_seen_at, last_indexed_at) are stored as Unix
// nanoseconds rather than milliseconds, so two index runs that complete
// within the same millisecond produce distinct cutoffs — important
// because PruneUnseen relies on strict-less-than comparison to detect
// stale rows.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/alehatsman/dex/internal/rerank"
	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // register sqlite3 driver
)

func init() {
	// Register the sqlite-vec extension so every new connection opened
	// by mattn/go-sqlite3 has vec0 / vec_distance_cosine available.
	sqlite_vec.Auto()
}

const (
	metaDim           = "dim"
	metaLastIndexedAt = "last_indexed_at"
	metaProjectRoot   = "project_root"
	metaEmbedModel    = "embed_model"
	metaVecQuant      = "vec_quant"
)

// ErrEmbedModelMismatch is returned by EnsureEmbedModel when the active
// embedding model differs from the one previously recorded for the
// index. Two same-dim models produce vectors in different latent spaces,
// so silently mixing them would corrupt retrieval — callers must rebuild
// the index (`dex reindex <path>`) before continuing.
var ErrEmbedModelMismatch = errors.New("embedding model mismatch")

// FusionMode controls how the dense and BM25 retrieval lanes are combined.
type FusionMode int

const (
	// FusionRRF fuses via Reciprocal Rank Fusion (default).
	FusionRRF FusionMode = iota
	// FusionLinear fuses via a convex combination on min-max normalised scores:
	//   α*dense_norm + (1-α)*bm25_norm
	// Tune α with DEX_FUSION_ALPHA; default 0.5. Set mode via DEX_FUSION_MODE=linear.
	FusionLinear
)

// FTSMode controls how buildFTSQuery joins tokens in the MATCH
// expression: AND for precision, OR for recall, Auto picks per-query.
type FTSMode int

const (
	// FTSModeAuto: AND when the query has 1–2 tokens (symbol-shaped
	// lookups), OR when it has 3+ tokens (natural-language questions
	// where strict AND too often returns zero hits).
	FTSModeAuto FTSMode = iota
	// FTSModeAND: every token must appear. Best precision, worst recall.
	FTSModeAND
	// FTSModeOR: any token can match. Best recall, worst precision —
	// bad hits get sunk by their BM25 rank in the fused score.
	FTSModeOR
)

// SearchOptions controls the lexical/semantic retrieval legs and score fusion.
type SearchOptions struct {
	// DisableBM25 turns off the lexical (FTS5/BM25) leg of hybrid
	// search. Useful for ablation / debugging the semantic ranking,
	// or for indexes built before the chunks_fts migration on a
	// truly old SQLite without FTS5 (unlikely).
	DisableBM25 bool

	// FTSMode picks the join operator for FTS tokens. Zero =
	// FTSModeAuto (recommended). See FTSMode for semantics.
	FTSMode FTSMode

	// MaxHitsPerFile, when > 0, caps results returned per unique file path.
	// Applied after ranking, before final truncation to k. Zero = no cap.
	MaxHitsPerFile int

	// DefinitionBoost is the multiplier applied in ApplyLocalRerank to
	// declaration-kind chunks (function/method/class/struct/…) for symbol
	// queries, lifting the definition site over window/orphan fragments that
	// merely mention the symbol. Zero = the default (defaultDefinitionBoost).
	// Tuned against the symbol-query eval set (see symbol_eval_test.go, #146).
	DefinitionBoost float64

	// FusionMode selects the score-fusion strategy for the dense+BM25 lanes.
	// FusionRRF uses Reciprocal Rank Fusion; FusionLinear uses a convex
	// combination on min-max normalised scores. The zero value is FusionRRF,
	// but the dex binary defaults to FusionLinear (calibrated in #317; set via
	// DEX_FUSION_MODE).
	FusionMode FusionMode

	// FusionAlpha is the dense-lane weight in FusionLinear mode (0 = pure
	// BM25, 1 = pure dense). Zero defaults to 0.7 (#317). Set via DEX_FUSION_ALPHA.
	FusionAlpha float32
}

// GraphOptions controls the graph-proximity spreading-activation lane.
type GraphOptions struct {
	// GraphGamma is the per-hop decay applied to the graph-proximity lane
	// during RRF fusion: a neighbor first reached at h hops contributes at
	// γ^h weight, so 1-hop callers outrank 3-hop ones (GraphCoder-style,
	// arXiv:2406.07003). Zero = the default (defaultGraphGamma). Set via
	// DEX_GRAPH_GAMMA. Tuned on the retrieval eval harness (#247/#248).
	GraphGamma float32

	// GraphHopCap bounds spreading-activation traversal depth — the context
	// blast-radius around matched symbols. Zero = the default
	// (defaultGraphHopCap). Set via DEX_GRAPH_HOP_CAP.
	GraphHopCap int

	// GraphLaneWeight is a flat multiplier on the graph-proximity RRF lane.
	// It scales the whole lane's contribution independently of the per-hop
	// γ decay, so raising it lets the graph lane compete with dense+BM25.
	// Zero = the default (defaultGraphLaneWeight = 1.0). Set via
	// DEX_GRAPH_WEIGHT. Useful range: 1–4; at 2× a 1-hop neighbor (γ=0.6)
	// contributes 1.2× the RRF score of a primary hit at the same rank.
	GraphLaneWeight float32
}

// RerankOptions configures the optional cross-encoder reranking stage.
type RerankOptions struct {
	// Reranker, when non-nil, reorders the fused candidate pool via a
	// cross-encoder before truncating to k. Nil = today's behaviour
	// (pure RRF). On rerank.ErrUnreachable the search falls back to
	// the pre-rerank order without surfacing an error to the caller.
	Reranker rerank.Reranker

	// RerankPool caps the fused candidate pool sent to the reranker.
	// Only honored when Reranker is non-nil. Zero = no cap (use the
	// natural pool size, max(5×k, 30)). Typical values 40–100: larger
	// = better recall but slower rerank call.
	RerankPool int

	// RerankTimeout is the per-call deadline applied to the rerank
	// request. A hung rerank endpoint must not stretch the whole `ask`
	// round-trip past the MCP timeout. Zero = 1500ms.
	RerankTimeout time.Duration

	// RerankCache, when non-nil, memoizes rerank results across calls
	// keyed on (query, sorted fused ids). Nil = the Store allocates a
	// 256-entry LRU on first use. Set explicitly to a sized cache to
	// override; pass a no-op cache to disable.
	RerankCache RerankCache
}

// InfraOptions controls storage and learning behaviour.
type InfraOptions struct {
	// DisableCoAccess turns off Hebbian co-access edge learning and spreading.
	// Set via DEX_COACCESS=0 or programmatically for isolated test stores.
	DisableCoAccess bool

	// VectorQuant selects the on-disk encoding of the chunk_vecs KNN index.
	// "" / "none" / "float32" = full-precision float32 (today's behavior);
	// "int8" = scalar-quantized int8 vectors (sqlite-vec `int8[dim]`, unit
	// range), ~4× smaller on disk and faster integer cosine, at a small
	// recall cost measured by `dex bench perf` / the retrieval eval (#215).
	// The full-precision vectors always stay in chunks.vec, so BM25-only
	// hits (scoreSemanticForIDs) and re-quantization keep full precision —
	// only the KNN MATCH leg is quantized. Set via DEX_VECTOR_QUANT.
	// Flipping the mode on an existing index rebuilds chunk_vecs from
	// chunks.vec on the next Open.
	VectorQuant string
}

// Options influence the runtime behaviour of an opened Store.
// All fields are optional; the zero value matches the default
// (hybrid BM25+semantic search enabled).
type Options struct {
	SearchOptions
	GraphOptions
	RerankOptions
	InfraOptions
}

type Store struct {
	db          *sql.DB
	dim         atomic.Int64 // vector dimension; set once on first upsert, read concurrently
	dimInit     sync.Mutex   // serializes first-write dim init so concurrent first UpsertMany calls don't double-init
	noVec       atomic.Bool  // true when index is BM25-only (DEX_EMBED_ENGINE=none) — no vec0 table, nil vecs
	embedModel  atomic.Value // string: model identity; "" until set by EnsureEmbedModel or recovered from meta
	opts        Options      // immutable after Open
	rerankCache RerankCache  // memoizes rerank results across calls; lazily set on first use
	rerankInit  sync.Once    // guards lazy rerankCache init

	knowledgeStore // knowledge-fact methods, keyed on Store.db
}

// Open opens or creates the SQLite file at path with default
// Options. Convenience wrapper around OpenWith.
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWith(ctx, path, Options{})
}

// OpenWith is like Open but lets the caller adjust runtime behaviour
// (e.g. disable the BM25 leg of hybrid search).
//
// `_busy_timeout=5000` lets concurrent writers (e.g. `dex index`
// fired while `dex watch` is also re-indexing) wait up to 5 s for
// the writer lock instead of immediately returning SQLITE_BUSY. Without
// it, racing index runs both crash with a leaked DDL error.
func OpenWith(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := sql.Open("sqlite3",
		"file:"+path+
			"?_journal_mode=WAL"+
			"&_synchronous=NORMAL"+
			"&_busy_timeout=5000"+
			"&_foreign_keys=1")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db, opts: opts, knowledgeStore: knowledgeStore{db: db}}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Recover the recorded vector dimension, if any.
	row := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='`+metaDim+`'`)
	var v string
	switch err := row.Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		// fresh db; dim discovered on first Upsert
	case err != nil:
		db.Close()
		return nil, err
	default:
		dim, _ := strconv.ParseInt(v, 10, 64)
		s.dim.Store(dim)
	}
	// Recover the recorded embed model identity. May be missing on
	// pre-migration indexes (built before this metadata existed) — those
	// adopt whatever model the first EnsureEmbedModel caller passes.
	row = db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='`+metaEmbedModel+`'`)
	var em string
	switch err := row.Scan(&em); {
	case errors.Is(err, sql.ErrNoRows):
		s.embedModel.Store("")
	case err != nil:
		db.Close()
		return nil, err
	default:
		s.embedModel.Store(em)
	}
	// Materialize the vec0 table now if we know the dim — covers both
	// brand-new opens (no chunks yet, dim known from a prior run) and
	// pre-vec0 indexes that need a one-shot backfill from chunks.vec.
	if err := s.ensureVecTable(ctx, s.dim.Load()); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure vec table: %w", err)
	}
	if err := s.ensureNodeVecTable(ctx, s.dim.Load()); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure node vec table: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

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

// migrateChunkContext adds context_text to chunks and updates the FTS5
// triggers to index context_text || content (Contextual BM25).
func (s *Store) migrateChunkContext(ctx context.Context) error {
	var done string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='chunk_context_added'`).Scan(&done)
	if done == "1" {
		return nil
	}
	// Add context_text column (no-op on duplicate).
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE chunks ADD COLUMN context_text TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("migrateChunkContext: add context_text: %w", err)
	}
	// Recreate FTS5 triggers so they index context_text || content.
	// DROP first because CREATE TRIGGER IF NOT EXISTS won't replace them.
	for _, drop := range []string{
		`DROP TRIGGER IF EXISTS chunks_ai`,
		`DROP TRIGGER IF EXISTS chunks_ad`,
		`DROP TRIGGER IF EXISTS chunks_au`,
	} {
		if _, err := s.db.ExecContext(ctx, drop); err != nil {
			return fmt.Errorf("migrateChunkContext: drop trigger: %w", err)
		}
	}
	for _, trig := range []string{
		`CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id,
		           CASE WHEN new.context_text IS NOT NULL AND new.context_text != ''
		                THEN new.context_text || CHAR(10) || new.content
		                ELSE new.content END,
		           new.path, new.kind);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES ('delete', old.id,
		           CASE WHEN old.context_text IS NOT NULL AND old.context_text != ''
		                THEN old.context_text || CHAR(10) || old.content
		                ELSE old.content END,
		           old.path, old.kind);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
		   INSERT INTO chunks_fts(chunks_fts, rowid, content, path, kind)
		   VALUES ('delete', old.id,
		           CASE WHEN old.context_text IS NOT NULL AND old.context_text != ''
		                THEN old.context_text || CHAR(10) || old.content
		                ELSE old.content END,
		           old.path, old.kind);
		   INSERT INTO chunks_fts(rowid, content, path, kind)
		   VALUES (new.id,
		           CASE WHEN new.context_text IS NOT NULL AND new.context_text != ''
		                THEN new.context_text || CHAR(10) || new.content
		                ELSE new.content END,
		           new.path, new.kind);
		 END`,
	} {
		if _, err := s.db.ExecContext(ctx, trig); err != nil {
			return fmt.Errorf("migrateChunkContext: create trigger: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('chunk_context_added', '1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		return fmt.Errorf("migrateChunkContext: flag: %w", err)
	}
	return nil
}

// quantMode returns the canonical chunk_vecs encoding for this store:
// "int8" when DEX_VECTOR_QUANT selects scalar quantization, "float32"
// otherwise. This canonical string is recorded in meta.vec_quant and
// compared on Open to detect a mode flip that needs a rebuild.
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
type Stats struct {
	Chunks    int
	Files     int
	Dim       int
	LastIndex time.Time
	// EmbedModel is the embedding model identity recorded for this
	// index. Empty for pre-migration indexes that pre-date the
	// embed_model meta key — those adopt the next caller's model on
	// the first EnsureEmbedModel call.
	EmbedModel string
}

// Dim reports the index's vector dimension (0 == BM25-only / no vectors yet).
// Cheap, lock-free read — used by callers that must decide whether the store
// will accept null-vector rows before attempting an upsert.
func (s *Store) Dim() int64 { return s.dim.Load() }

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	st.Dim = int(s.dim.Load())
	if v, ok := s.embedModel.Load().(string); ok {
		st.EmbedModel = v
	}
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT path) FROM chunks`)
	if err := row.Scan(&st.Chunks, &st.Files); err != nil {
		return st, err
	}
	row = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='`+metaLastIndexedAt+`'`)
	var v string
	if err := row.Scan(&v); err == nil {
		ts, _ := strconv.ParseInt(v, 10, 64)
		if ts > 0 {
			st.LastIndex = time.Unix(0, ts)
		}
	}
	return st, nil
}

// FileEntry is one indexed file with its chunk count.
type FileEntry struct {
	Path   string
	Chunks int
}

// FileTree returns indexed files whose path starts with prefix, with per-file
// chunk counts ordered by path. Pass "" to list all files in the project.
func (s *Store) FileTree(ctx context.Context, prefix string) ([]FileEntry, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if prefix == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT path, COUNT(*) FROM chunks GROUP BY path ORDER BY path`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT path, COUNT(*) FROM chunks WHERE path LIKE ? ESCAPE '\' GROUP BY path ORDER BY path`,
			escapeLike(prefix)+"/%")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileEntry
	for rows.Next() {
		var f FileEntry
		if err := rows.Scan(&f.Path, &f.Chunks); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetLastIndexedAt records the wall-clock time of the most recent
// successful (full or incremental) re-index.
func (s *Store) SetLastIndexedAt(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('`+metaLastIndexedAt+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(t.UnixNano(), 10))
	return err
}

// EmbedModel returns the embedding model identity previously recorded
// for this index, or "" if none has been recorded yet.
func (s *Store) EmbedModel() string {
	v, _ := s.embedModel.Load().(string)
	return v
}

// EnsureEmbedModel records the active embedding model identity (e.g.
// "Qwen3-Embedding-4B") and refuses subsequent runs that pass a
// different identity. Pre-migration indexes (no embed_model row) adopt
// the first caller's model.
//
// Returns ErrEmbedModelMismatch wrapped with the recorded and active
// names when they disagree — callers should surface the wrapped error
// to the user with a `dex reindex <path>` hint. Passing an empty name
// is a no-op (callers without a model identity skip the gate).
func (s *Store) EnsureEmbedModel(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	prev, _ := s.embedModel.Load().(string)
	if prev == name {
		return nil
	}
	if prev != "" {
		return fmt.Errorf("%w: index was built with %q, current run uses %q — run `dex reindex` to rebuild", ErrEmbedModelMismatch, prev, name)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('`+metaEmbedModel+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, name); err != nil {
		return err
	}
	s.embedModel.Store(name)
	return nil
}

// SetProjectRoot records the absolute project path this index belongs
// to. Needed by `reindex --all`, which walks the sha256(path)-keyed
// cache dirs and has to recover each project's original on-disk root.
func (s *Store) SetProjectRoot(ctx context.Context, root string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('`+metaProjectRoot+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, root)
	return err
}

// ProjectRoot returns the path previously recorded by SetProjectRoot.
// Returns "" (not an error) if the row is missing — that's the
// pre-migration case for indexes built before this metadata existed.
func (s *Store) ProjectRoot(ctx context.Context) (string, error) {
	var v string
	row := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='`+metaProjectRoot+`'`)
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// ExistingSHAs returns the set of content_sha1 already present for path,
// so the indexer can skip re-embedding unchanged chunks.
func (s *Store) ExistingSHAs(ctx context.Context, path string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_sha1 FROM chunks WHERE path=?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

// ExistingSHAsBatch returns existing content_sha1 sets for multiple paths in a
// single round-trip. The outer map is keyed by path; missing paths map to nil.
// Batched in groups of 500 to stay within SQLite's default parameter limit.
func (s *Store) ExistingSHAsBatch(ctx context.Context, paths []string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool, len(paths))
	const batchSize = 500
	for i := 0; i < len(paths); i += batchSize {
		end := i + batchSize
		if end > len(paths) {
			end = len(paths)
		}
		slice := paths[i:end]
		args := make([]any, len(slice))
		for j, p := range slice {
			args[j] = p
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT path, content_sha1 FROM chunks WHERE path IN (`+inPlaceholders(len(slice))+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var path, sha string
			if err := rows.Scan(&path, &sha); err != nil {
				rows.Close()
				return nil, err
			}
			if out[path] == nil {
				out[path] = make(map[string]bool)
			}
			out[path][sha] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// PendingChunk is one row destined for an UpsertMany batch.
type PendingChunk struct {
	Path       string
	Kind       string
	Name       string
	StartLine  int
	EndLine    int
	ContentSHA string
	Content    string
	Vec        []float32
}

// initDim performs the one-time, first-write index initialization:
// persist the vector dimension to meta and materialize the vec0 table +
// mirror triggers. It is safe to call concurrently — dimInit serializes
// callers and the double-check skips the work once another goroutine has
// completed it. The dim is published to s.dim only after ensureVecTable
// succeeds, so a concurrent reader that sees s.dim != 0 is guaranteed the
// vec0 triggers already exist.
func (s *Store) initDim(ctx context.Context, dim int64) error {
	s.dimInit.Lock()
	defer s.dimInit.Unlock()
	if s.dim.Load() != 0 { // another goroutine already initialized
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('`+metaDim+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(dim, 10)); err != nil {
		return err
	}
	if err := s.ensureVecTable(ctx, dim); err != nil {
		return err
	}
	if err := s.ensureNodeVecTable(ctx, dim); err != nil {
		return err
	}
	s.dim.Store(dim)
	return nil
}

// UpsertMany inserts a batch of chunks in a single transaction. One
// commit per batch instead of one commit per chunk drops the no-op
// fsync count by ~32× on a typical run and is well worth the slight
// API duplication.
func (s *Store) UpsertMany(ctx context.Context, rows []PendingChunk, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	if s.dim.Load() == 0 && !s.noVec.Load() {
		if len(rows[0].Vec) == 0 {
			// BM25-only mode: no embedder, no vec0 table. Mark once under
			// dimInit so subsequent calls skip this block on the fast path.
			s.dimInit.Lock()
			if s.dim.Load() == 0 && !s.noVec.Load() {
				s.noVec.Store(true)
			}
			s.dimInit.Unlock()
		} else {
			// First write to a fresh vector index. Serialize init under dimInit so
			// that `dex index` and `dex watch` racing their first UpsertMany
			// don't both write meta.dim / build the vec0 table. dim is
			// published to the atomic only after the vec0 table + triggers
			// exist, so any reader that observes dim != 0 can safely INSERT
			// into chunks and rely on the mirror triggers being present.
			if err := s.initDim(ctx, int64(len(rows[0].Vec))); err != nil {
				return err
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks(path, kind, name, start_line, end_line, content_sha1, content, vec, last_seen_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path, content_sha1) DO UPDATE SET
		   kind=excluded.kind,
		   name=excluded.name,
		   start_line=excluded.start_line,
		   end_line=excluded.end_line,
		   content=excluded.content,
		   vec=excluded.vec,
		   last_seen_at=excluded.last_seen_at`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if !s.noVec.Load() && int64(len(r.Vec)) != s.dim.Load() {
			_ = tx.Rollback()
			return fmt.Errorf("vector dim mismatch: index has dim=%d, got %d (did the embedding model change?)", s.dim.Load(), len(r.Vec))
		}
		if _, err := stmt.ExecContext(ctx,
			r.Path, r.Kind, r.Name, r.StartLine, r.EndLine, r.ContentSHA, r.Content,
			encodeVec(r.Vec), now.UnixNano()); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// TouchSeen bumps last_seen_at for an already-present (path, sha) pair and
// backfills the name column — so chunks indexed before the name column was
// added get their names populated on the next walk without re-embedding.
//
// When startLine > 0, also refreshes start_line/end_line. Required for
// the chunker fast-path: a chunk's content can stay byte-identical
// (same SHA) while its position in the file shifts because some earlier
// UpsertChunkContext stores a Contextual Retrieval situating sentence and
// the newly re-embedded vector for an existing chunk identified by
// (path, content_sha1). The UPDATE fires the chunks_au trigger, which
// re-indexes the chunk in FTS5 with context_text || content — that is
// the Contextual BM25 gain. The new vector provides the dense embedding
// gain (chunk re-embedded with EmbedTextWithContext).
//
// No-op when no row matches (chunk was pruned since the job was queued).
func (s *Store) UpsertChunkContext(ctx context.Context, path, contentSHA, contextText string, newVec []float32) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chunks SET context_text = ?, vec = ?
		 WHERE path = ? AND content_sha1 = ?`,
		contextText, encodeVec(newVec), path, contentSHA)
	if err != nil {
		return fmt.Errorf("UpsertChunkContext: %w", err)
	}
	return nil
}

// chunk in the same file grew or shrank. Without this update, search_symbol
// returns the chunk's ORIGINAL line range even after the file was edited
// above it. Callers that don't have line info (file/package/repo summary
// touches) pass 0 to skip the position update.
func (s *Store) TouchSeen(ctx context.Context, path, contentSHA, name string, startLine, endLine int, now time.Time) error {
	if startLine > 0 {
		_, err := s.db.ExecContext(ctx,
			`UPDATE chunks SET last_seen_at=?, name=?, start_line=?, end_line=? WHERE path=? AND content_sha1=?`,
			now.UnixNano(), name, startLine, endLine, path, contentSHA)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE chunks SET last_seen_at=?, name=? WHERE path=? AND content_sha1=?`,
		now.UnixNano(), name, path, contentSHA)
	return err
}

// TouchPath bumps last_seen_at for every chunk of a single file in one
// statement. Used by the mtime fast-path: when a file hasn't changed
// since the previous successful index, we don't need to read it or
// re-chunk it — we just have to mark its chunks live so PruneUnseen
// doesn't drop them. Returns the number of rows touched (0 means the
// file has no chunks yet — caller must fall back to the slow path).
func (s *Store) TouchPath(ctx context.Context, path string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chunks SET last_seen_at=? WHERE path=?`,
		now.UnixNano(), path)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneUnseen deletes chunks last seen before `cutoff`. Call at the end of a
// re-index to remove stale rows for files that disappeared.
func (s *Store) PruneUnseen(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chunks
		   WHERE last_seen_at < ?`,
		cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SeenTime returns the timestamp an index Run should stamp its chunks
// with — and prune by. It is max(now, latest stored last_seen_at + 1ns)
// over the chunks table, so a run's stamp/cutoff strictly exceeds every
// previously stored stamp even when the wall clock steps backward (NTP /
// VM clock resync — common on WSL2).
//
// Without this, a backward step makes a later run's cutoff numerically
// smaller than the rows a prior run stamped, so PruneUnseen's strict
// `last_seen_at < cutoff` comparison leaves deleted files' chunks
// un-pruned (dex #32). The caller must read this BEFORE upserting (the
// upsert bumps the max) and use the one value for every UpsertMany /
// MarkSeen / PruneUnseen call in the run.
func (s *Store) SeenTime(ctx context.Context, now time.Time) (time.Time, error) {
	var maxSeen sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(last_seen_at) FROM chunks`).Scan(&maxSeen); err != nil {
		return time.Time{}, err
	}
	ns := now.UnixNano()
	if maxSeen.Valid && maxSeen.Int64 >= ns {
		ns = maxSeen.Int64 + 1
	}
	return time.Unix(0, ns), nil
}

// DeletePath drops all chunks for a single relative path.
func (s *Store) DeletePath(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chunks WHERE path=?`, path); err != nil {
		return err
	}
	return nil
}

// DeletePathPrefix drops all chunks whose path starts with prefix.
// Used by the indexer to evict chunks under a directory that has
// become ignored between runs (e.g. a fresh `node_modules/` entry).
func (s *Store) DeletePathPrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM chunks WHERE path LIKE ? ESCAPE '\'`,
		escapeLike(prefix)+`%`); err != nil {
		return err
	}
	return nil
}

// escapeLike escapes the LIKE-pattern metacharacters in s.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// Hit is one search result.
type Hit struct {
	Path      string
	Kind      string
	Name      string
	StartLine int
	EndLine   int
	Content   string

	// Score is the cosine similarity in [-1, 1] (1.0 == identical
	// direction). Always populated, even for hits that surfaced via
	// the BM25 path — useful as a familiar "is this close?" number
	// for humans and for downstream filtering.
	Score float32

	// BM25Score is the FTS5 bm25() rank when the hit surfaced through
	// the lexical path. SQLite returns these as small negative
	// numbers (more negative = better); we negate so larger = better.
	// Zero when the hit didn't match the BM25 query at all.
	BM25Score float32

	// RRFScore is the fused rank used for ordering when hybrid search
	// is active: 1/(60+sem_rank) + 1/(60+bm25_rank). Zero when search
	// ran semantic-only (empty query text or DEX_DISABLE_BM25=1).
	RRFScore float32

	// RerankScore is the cross-encoder relevance score in [0, 1] for
	// the (query, chunk) pair. Zero when rerank didn't run (no client
	// wired, pool ≤ k, or endpoint unreachable). Larger = more relevant.
	RerankScore float32

	// Centrality fields — populated from graph_nodes via the
	// chunk_id join when the symbol has a corresponding graph node.
	// Zero when no graph node exists (the file is in an unindexed
	// language, the chunk isn't a function/method, or the graph hasn't
	// been built yet). Callers use these to sort and to compose the
	// role-hint shown to agents.
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	PageRank        float64
	Betweenness     float64
}

// FormatHits renders a slice of hits as a fenced CONTEXT block for
// injection into a chat completion message. Each chunk gets a header
// with path:line coordinates so the model can cite real locations.
func FormatHits(hits []Hit) string {
	var b strings.Builder
	b.WriteString("CONTEXT — relevant chunks from the project's dex index:\n\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "--- chunk %d: %s:%d-%d (%s, score=%.4f) ---\n",
			i+1, h.Path, h.StartLine, h.EndLine, h.Kind, h.Score)
		b.WriteString(h.Content)
		if !strings.HasSuffix(h.Content, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// scored holds one chunk's score during ranking. Used internally by
// both the semantic and BM25 legs; the RRF fuser then walks both lists.
type scored struct {
	id    int64
	score float32 // cosine for semantic; -bm25() for BM25 (larger = better)
}

// rrfK is the RRF dampening constant. 60 is the canonical default from
// Cormack et al. (2009); behavior is robust to values in [10, 100].
const rrfK = 60

// queryType classifies an incoming query to drive adaptive RRF weights.
// SACL (EMNLP 2025): query structure is a strong signal for which retrieval
// modality (lexical vs dense) dominates. Weights below are empirically tuned:
//
//	Symbol:       BM25 1.4 × dense 0.6  — exact token match dominates
//	Architecture: BM25 0.6 × dense 1.4  — semantic similarity dominates
//	NL (default): BM25 1.0 × dense 1.0  — equal contribution
type queryType int

const (
	queryNL           queryType = iota
	querySymbol                 // CamelCase / snake_case / qualified names
	queryArchitecture           // "how does", "architecture", "data flow", …
)

// QueryTypeSymbol / QueryTypeNL / QueryTypeArchitecture are the exported
// constants returned by ClassifyQueryType.
const (
	QueryTypeNL           = "nl"
	QueryTypeSymbol       = "symbol"
	QueryTypeArchitecture = "architecture"
)

// ClassifyQueryType is the exported variant of classifyQueryType.
func ClassifyQueryType(q string) string {
	switch classifyQueryType(q) {
	case querySymbol:
		return QueryTypeSymbol
	case queryArchitecture:
		return QueryTypeArchitecture
	default:
		return QueryTypeNL
	}
}

// classifyQueryType returns the queryType for q using lightweight heuristics.
// NL is the safe default when neither Symbol nor Architecture patterns fire.
func classifyQueryType(q string) queryType {
	q = strings.TrimSpace(q)
	if q == "" {
		return queryNL
	}
	lower := strings.ToLower(q)

	// Architecture: multi-token phrases about structure/design.
	archPhrases := []string{
		"how does", "how is", "where is", "where are",
		"architecture", "design pattern", "data flow", "control flow",
		"module structure", "component", "pipeline", "layer",
	}
	for _, p := range archPhrases {
		if strings.Contains(lower, p) {
			return queryArchitecture
		}
	}

	// Symbol: single token that looks like a code identifier.
	// Fire only when the entire query is one token (no whitespace except
	// qualifiers like "Foo::Bar" or "obj.method").
	fields := strings.Fields(q)
	if len(fields) == 1 {
		tok := fields[0]
		if looksLikeIdentifier(tok) {
			return querySymbol
		}
	}
	// Two-token queries where both tokens are identifiers (e.g. "Store Search").
	if len(fields) == 2 && looksLikeIdentifier(fields[0]) && looksLikeIdentifier(fields[1]) {
		return querySymbol
	}

	return queryNL
}

// looksLikeIdentifier returns true for tokens that match common code
// identifier patterns: CamelCase, PascalCase, snake_case, SCREAMING_CASE,
// qualified names (Foo::bar, obj.method, (*T).Method), private _foo.
func looksLikeIdentifier(tok string) bool {
	if len(tok) == 0 {
		return false
	}
	// Strip leading sigils (* & ( )).
	stripped := strings.TrimLeft(tok, "(*&")
	stripped = strings.TrimRight(stripped, ")")
	if stripped == "" {
		return false
	}
	// Must contain only identifier runes plus qualifiers . :: _ -
	for _, r := range stripped {
		if !isIdentRune(r) {
			return false
		}
	}
	// Contains at least one uppercase letter, underscore, or qualifier
	// (avoids matching plain lowercase words like "go" or "run").
	hasUpper := strings.IndexFunc(stripped, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0
	hasQual := strings.ContainsAny(stripped, "._:")
	hasUnderscore := strings.Contains(stripped, "_") && len(stripped) > 3
	return hasUpper || hasQual || hasUnderscore
}

func isIdentRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '-' ||
		r == '(' || r == ')' || r == '*' || r == '&'
}

// fuseLinear combines dense and BM25 scores via a min-max normalised convex
// combination:  alpha*dense_norm + (1-alpha)*bm25_norm.
// Both maps use the convention "higher = better" (BM25 scores are already
// negated before reaching here via scoreBM25).
// Items absent from a lane receive 0 after normalisation (bottom of that
// lane's range), which is conservative and avoids rewarding absent signals.
func fuseLinear(semCosine, bm25Score map[int64]float32, alpha float32) map[int64]float32 {
	if alpha <= 0 {
		alpha = 0.7
	}
	dMin, dMax := mapMinMax(semCosine)
	bMin, bMax := mapMinMax(bm25Score)

	out := make(map[int64]float32, len(semCosine)+len(bm25Score))
	for id, v := range semCosine {
		out[id] += alpha * minMaxNorm(v, dMin, dMax)
	}
	for id, v := range bm25Score {
		out[id] += (1 - alpha) * minMaxNorm(v, bMin, bMax)
	}
	return out
}

func mapMinMax(m map[int64]float32) (lo, hi float32) {
	first := true
	for _, v := range m {
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	return
}

func minMaxNorm(v, lo, hi float32) float32 {
	if hi == lo {
		return 0
	}
	return (v - lo) / (hi - lo)
}

// rrfWeights returns the BM25 and dense RRF multiplicative weights for qt.
func rrfWeights(qt queryType) (bm25W, denseW float32) {
	switch qt {
	case querySymbol:
		return 1.4, 0.6
	case queryArchitecture:
		return 0.6, 1.4
	default:
		return 1.0, 1.0
	}
}

// Search returns the top-k chunks ranked by hybrid scoring with optional
// per-file diversity via Options.MaxHitsPerFile. The canonical local quality
// rerank (noise/definition/coherence/MMR) is applied exactly once, here.
//
// Graph expansion runs BEFORE the cross-encoder rerank so graph-boosted
// semantic hits (files in the semantic pool that are also graph-adjacent) get
// the benefit of the extra RRF score. However, pure graph-only hits — files
// that are NOT in the original semantic pool — are excluded from the reranker
// and appended as breadth-only tail additions. This prevents the content-aware
// cross-encoder (which gained real text for graph hits after #361) from
// promoting graph-only files above true semantic gold files. (#394)
func (s *Store) Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	// Over-fetch to give the graph-fuse and rerank stages headroom.
	candidateK := k * 5
	if candidateK < 30 {
		candidateK = 30
	}
	hits, err := s.SearchFused(ctx, queryVec, queryText, candidateK)
	if err != nil || len(hits) == 0 {
		return hits, err
	}

	// Record semantic-origin paths so we can separate them from pure graph
	// additions after the expansion step.
	semanticPaths := make(map[string]struct{}, len(hits))
	for i := range hits {
		semanticPaths[hits[i].Path] = struct{}{}
	}

	// Graph expansion: boosts semantic hits that are also graph-adjacent and
	// adds graph-only neighbors for breadth.
	hits = s.FuseSpreadingActivation(ctx, hits, queryVec, candidateK)

	// Split merged pool: semantic-origin hits go to the cross-encoder; pure
	// graph-only hits are held aside to avoid cross-encoder crowding. (#394)
	rerankPool := hits[:0:0]
	var graphTail []Hit
	for _, h := range hits {
		if _, ok := semanticPaths[h.Path]; ok {
			rerankPool = append(rerankPool, h)
		} else {
			graphTail = append(graphTail, h)
		}
	}

	rerankPool, err = s.RerankFused(ctx, queryText, rerankPool, k)
	if err != nil {
		return nil, err
	}

	hits = append(rerankPool, graphTail...)
	if s.opts.MaxHitsPerFile > 0 {
		hits = diversify(hits, s.opts.MaxHitsPerFile)
	}
	return hits, nil
}

// SearchFused returns RRF-fused (+ session-proximity) candidates WITHOUT the
// local quality rerank or the cross-encoder pass. Callers that fuse additional
// retrieval legs (exact-symbol lookup, graph neighbors) use this and then call
// ApplyLocalRerank once over the combined set, so the quality rerank runs
// exactly once — never twice, as happened when these callers stacked their own
// rerank on top of Store.Search's.
func (s *Store) SearchFused(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error) {
	return s.searchRaw(ctx, queryVec, queryText, k, false)
}

// RerankFused is the single rerank entry point for callers that fuse their own
// extra retrieval legs onto a SearchFused pool (the mcp search tools). It runs
// the cross-encoder over the final union when a reranker is wired and the pool
// exceeds k — its ordering is authoritative and it populates Hit.RerankScore —
// and otherwise (or on a reranker outage) falls back to the canonical local
// quality rerank. Trims to k. This guarantees the rerank runs exactly once over
// the complete candidate set, regardless of which legs the caller fused in.
func (s *Store) RerankFused(ctx context.Context, queryText string, hits []Hit, k int) ([]Hit, error) {
	if k <= 0 {
		k = 8
	}
	if s.opts.Reranker != nil && len(hits) > k {
		docs := make([]string, len(hits))
		for i := range hits {
			docs[i] = hits[i].Content
		}
		// In-process LRU keyed on (query, ordered docs). Interactive sessions
		// re-issue the same query repeatedly, and the cross-encoder call is the
		// most expensive leg — an identical (query, pool) returns the prior
		// scores without a second network call. (The scored Store.Search path
		// caches in s.rerank; this is the equivalent for the fused path, which
		// regressed when Store.Search was routed through RerankFused — #191.)
		cache := s.getRerankCache()
		cacheKey := rerankDocsCacheKey(queryText, docs)
		var (
			scores []rerank.Score
			err    error
		)
		if cached, ok := cache.Get(cacheKey); ok && cached.scores != nil {
			scores = cached.scores
		} else {
			scores, err = s.rerankDocs(ctx, queryText, docs)
			if err == nil {
				cache.Put(cacheKey, rerankCached{scores: scores})
			}
		}
		switch {
		case err == nil:
			ordered := make([]Hit, 0, len(scores))
			for _, sc := range scores {
				if sc.Index < 0 || sc.Index >= len(hits) {
					continue
				}
				h := hits[sc.Index]
				h.RerankScore = sc.Score
				ordered = append(ordered, h)
			}
			if len(ordered) > k {
				ordered = ordered[:k]
			}
			return ordered, nil
		case errors.Is(err, rerank.ErrUnreachable):
			// reranker outage — fall through to the local quality rerank
		default:
			return nil, err
		}
	}
	out := ApplyLocalRerank(hits, classifyQueryType(queryText) == querySymbol, s.opts.DefinitionBoost)
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// diversify caps the number of hits per unique file path, preserving
// the existing score-based ordering. Hits beyond the cap are dropped.
func diversify(hits []Hit, maxPerFile int) []Hit {
	counts := make(map[string]int, len(hits)/2)
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if counts[h.Path] >= maxPerFile {
			continue
		}
		counts[h.Path]++
		out = append(out, h)
	}
	return out
}

// searchRaw is the internal search implementation. See Search for the
// public API. When `queryText` is non-empty AND BM25 isn't disabled,
// results from the cosine path and the FTS5/BM25 path are fused via
// Reciprocal Rank Fusion: rrf_score(id) = Σ 1/(60+rank_in_list). RRF
// is scale-free, so the two heterogenous scoring schemes compose without
// per-corpus tuning. When `queryText` is empty (or BM25 disabled),
// search degrades to semantic-only.
//
// When applyLocal is true the canonical ApplyLocalRerank (noise / definition /
// coherence / MMR) and the optional cross-encoder pass run here. When false the
// fused candidates are returned untouched so a caller can fuse extra legs and
// rerank the union exactly once (see SearchFused).
func (s *Store) searchRaw(ctx context.Context, queryVec []float32, queryText string, k int, applyLocal bool) ([]Hit, error) {
	if k <= 0 {
		k = 8
	}
	useBM25 := !s.opts.DisableBM25 && strings.TrimSpace(queryText) != ""

	if !useBM25 {
		// Semantic-only path. vec0 already returns rows sorted by
		// similarity desc, so no client-side sort needed.
		semScores, err := s.scoreSemantic(ctx, queryVec, k)
		if err != nil {
			return nil, err
		}
		if len(semScores) == 0 {
			return nil, nil
		}
		hits, err := s.fetchHits(ctx, semScores, scoreContext{})
		if err != nil {
			return nil, err
		}
		if applyLocal {
			hits = ApplyLocalRerank(hits, classifyQueryType(queryText) == querySymbol, s.opts.DefinitionBoost)
			if len(hits) > k {
				hits = hits[:k]
			}
		}
		return hits, nil
	}

	// Pull more candidates per leg than the final k so fusion has
	// headroom to surface lexical-only or semantic-only hits.
	pool := k * 5
	if pool < 30 {
		pool = 30
	}
	// When a reranker is wired, cap the pool so we don't pay
	// cross-encoder cost on more docs than the operator chose.
	if s.opts.Reranker != nil && s.opts.RerankPool > 0 && pool > s.opts.RerankPool {
		pool = s.opts.RerankPool
	}

	// Semantic top-pool — sqlite-vec KNN, already sorted desc by similarity.
	semSorted, err := s.scoreSemantic(ctx, queryVec, pool)
	if err != nil {
		return nil, err
	}
	semCosine := make(map[int64]float32, len(semSorted))
	semRank := make(map[int64]int, len(semSorted))
	for i, sc := range semSorted {
		semCosine[sc.id] = sc.score
		semRank[sc.id] = i + 1
	}

	// BM25 top-pool.
	bm25Scores, err := s.scoreBM25(ctx, queryText, pool)
	if err != nil {
		// If FTS5 chokes on the query (e.g. unbalanced quotes), fall
		// back to semantic-only rather than failing the user's search.
		bm25Scores = nil
	}
	bm25Rank := make(map[int64]int, len(bm25Scores))
	bm25Score := make(map[int64]float32, len(bm25Scores))
	for i, sc := range bm25Scores {
		bm25Rank[sc.id] = i + 1
		bm25Score[sc.id] = sc.score
	}

	// Fill cosine for BM25-only fused IDs so Hit.Score stays populated
	// for every result, not just semantic-leg ones. The set is bounded
	// by `pool` and usually small in practice (high lexical/semantic
	// overlap), so the extra round-trip is cheap.
	var missing []int64
	for id := range bm25Rank {
		if _, ok := semCosine[id]; !ok {
			missing = append(missing, id)
		}
	}
	if filled, err := s.scoreSemanticForIDs(ctx, queryVec, missing); err == nil {
		for id, sim := range filled {
			semCosine[id] = sim
		}
	}

	// Fuse dense and BM25 lanes.
	var rrf map[int64]float32
	if s.opts.FusionMode == FusionLinear {
		// Convex combination on min-max normalised scores.
		// alpha (DEX_FUSION_ALPHA) is the dense weight; 0 defaults to 0.7 (#317).
		rrf = fuseLinear(semCosine, bm25Score, s.opts.FusionAlpha)
	} else {
		// Weighted RRF. Weights are query-type-adaptive (SACL EMNLP 2025):
		// symbol queries favour lexical, architecture queries favour dense,
		// NL queries use equal weights. Scale-free property is preserved —
		// the constant multipliers cancel in relative ranking.
		bm25W, denseW := rrfWeights(classifyQueryType(queryText))
		rrf = make(map[int64]float32, len(semRank)+len(bm25Rank))
		for id, r := range semRank {
			rrf[id] += denseW / float32(rrfK+r)
		}
		for id, r := range bm25Rank {
			rrf[id] += bm25W / float32(rrfK+r)
		}
	}

	// Batch-fetch paths for the full fused pool — used by noise penalties,
	// session proximity boost, and MMR diversity below. Fast PK lookup.
	allIDs := make([]int64, 0, len(rrf))
	for id := range rrf {
		allIDs = append(allIDs, id)
	}
	pathFor, _ := s.fetchPathsForIDs(ctx, allIDs) // degrade gracefully on error

	// Note: noise penalties / definition / coherence / MMR are NOT applied
	// here — they belong to the single canonical reranker (ApplyLocalRerank),
	// invoked once at the tail when applyLocal is set. Applying them inline
	// here too would double-penalize callers that rerank again downstream.

	// Session graph proximity boost (#118): files the agent recently
	// touched in this session get an extra RRF addend, making search
	// context-aware without explicit path filtering.
	s.applyProximityBonus(ctx, rrf, pathFor)

	fused := make([]scored, 0, len(rrf))
	for id, r := range rrf {
		fused = append(fused, scored{id, r})
	}
	sort.Slice(fused, func(i, j int) bool { return fused[i].score > fused[j].score })

	// SearchFused path: hand back the FULL fused candidate pool (k*5, not
	// trimmed to k) without the cross-encoder or local rerank, so the caller
	// can fuse additional legs and then rerank the union exactly once via
	// RerankFused. Trimming here would starve the downstream cross-encoder of
	// the candidate pool it needs.
	if !applyLocal {
		return s.fetchHits(ctx, fused, scoreContext{semCosine: semCosine, bm25Score: bm25Score})
	}

	// Cross-encoder rerank: only fires if a client is wired and we actually
	// have more candidates than k (otherwise reordering is a no-op). Its
	// ordering is authoritative, so it returns directly. On ErrUnreachable,
	// fall through to the local rerank so reranker outages never surface as
	// search failures.
	if s.opts.Reranker != nil && len(fused) > k {
		reranked, rerankScore, err := s.rerank(ctx, queryText, fused, k)
		switch {
		case err == nil:
			return s.fetchHits(ctx, reranked, scoreContext{semCosine: semCosine, bm25Score: bm25Score, rrfScore: rrf, rerankScore: rerankScore})
		case errors.Is(err, rerank.ErrUnreachable):
			// fall through to local rerank
		default:
			return nil, err
		}
	}

	// Canonical local rerank over the full fused pool, then trim to k.
	hits, err := s.fetchHits(ctx, fused, scoreContext{semCosine: semCosine, bm25Score: bm25Score})
	if err != nil {
		return nil, err
	}
	hits = ApplyLocalRerank(hits, classifyQueryType(queryText) == querySymbol, s.opts.DefinitionBoost)
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// inPlaceholders returns a comma-separated list of n SQL "?" bind vars,
// e.g. inPlaceholders(3) == "?,?,?".
func inPlaceholders(n int) string {
	s := strings.Repeat("?,", n)
	return s[:len(s)-1]
}

// rerankDocs delegates to the configured reranker under the per-call deadline
// (Options.RerankTimeout, default 1500ms) and maps a deadline expiry to
// rerank.ErrUnreachable so callers degrade to the pre-rerank ordering instead
// of surfacing a hard search failure. Shared by the scored-based rerank (simple
// Store.Search path, with id-keyed LRU cache) and the Hit-based RerankFused
// (fusing callers) so the deadline + error semantics live in one place.
func (s *Store) rerankDocs(ctx context.Context, queryText string, docs []string) ([]rerank.Score, error) {
	timeout := s.opts.RerankTimeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	rerankCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scores, err := s.opts.Reranker.Rerank(rerankCtx, queryText, docs)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w: rerank timed out after %s", rerank.ErrUnreachable, timeout)
		}
		return nil, err
	}
	return scores, nil
}

// rerank fetches `Content` for the fused pool, sends (query, docs) to
// the reranker, maps the returned indices back to chunk IDs, and
// returns the top-k slice together with a per-id rerank score map.
//
// Two safeguards beyond the bare delegation:
//   - Per-call deadline derived from Options.RerankTimeout (default 1500ms).
//     A hung rerank endpoint must not stretch the whole `ask` round-trip
//     past the MCP timeout. Deadline expiry is wrapped as
//     rerank.ErrUnreachable so the caller's existing fallback triggers.
//   - In-process LRU keyed on (query, sorted fused ids). Interactive
//     sessions iterate on the same query repeatedly; the cache avoids
//     paying the rerank network call for an identical (query, id-set).
func (s *Store) rerank(ctx context.Context, queryText string, fused []scored, k int) ([]scored, map[int64]float32, error) {
	if len(fused) == 0 {
		return nil, nil, nil
	}

	// Cache lookup: identical (query, id-set) returns the prior result.
	cache := s.getRerankCache()
	ids := make([]int64, len(fused))
	for i, sc := range fused {
		ids[i] = sc.id
	}
	key := rerankCacheKey(queryText, ids)
	if cached, ok := cache.Get(key); ok {
		out := cached.scored
		if len(out) > k {
			out = out[:k]
		}
		return out, cached.rerankScore, nil
	}

	idArgs := make([]any, len(fused))
	for i, sc := range fused {
		idArgs[i] = sc.id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content FROM chunks WHERE id IN (`+inPlaceholders(len(idArgs))+`)`,
		idArgs...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	contentByID := make(map[int64]string, len(fused))
	for rows.Next() {
		var id int64
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, nil, err
		}
		contentByID[id] = content
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Build docs in fused order so rerank.Score.Index maps cleanly back.
	docs := make([]string, 0, len(fused))
	docIDs := make([]int64, 0, len(fused))
	for _, sc := range fused {
		c, ok := contentByID[sc.id]
		if !ok {
			continue // chunk vanished between fusion and content fetch
		}
		docs = append(docs, c)
		docIDs = append(docIDs, sc.id)
	}

	scores, err := s.rerankDocs(ctx, queryText, docs)
	if err != nil {
		return nil, nil, err
	}

	reranked := make([]scored, 0, len(scores))
	rerankScore := make(map[int64]float32, len(scores))
	for _, sc := range scores {
		if sc.Index < 0 || sc.Index >= len(docIDs) {
			continue
		}
		id := docIDs[sc.Index]
		reranked = append(reranked, scored{id: id, score: sc.Score})
		rerankScore[id] = sc.Score
	}
	// Cache the full ranked slice (before k truncation) so a follow-up
	// query for a different k against the same id-set still benefits.
	cache.Put(key, rerankCached{scored: append([]scored(nil), reranked...), rerankScore: rerankScore})
	if len(reranked) > k {
		reranked = reranked[:k]
	}
	return reranked, rerankScore, nil
}

// getRerankCache returns the configured RerankCache, lazily allocating
// a default 256-entry LRU on first call.
func (s *Store) getRerankCache() RerankCache {
	s.rerankInit.Do(func() {
		if s.opts.RerankCache != nil {
			s.rerankCache = s.opts.RerankCache
		} else {
			s.rerankCache = newRerankLRU(256)
		}
	})
	return s.rerankCache
}

// scoreSemantic returns up to `limit` chunks ranked by cosine similarity
// to queryVec, best first. Runs as a single KNN query against the
// sqlite-vec `chunk_vecs` virtual table; vec0 returns rows sorted by
// distance ascending, which is similarity descending — no client-side
// sort needed.
func (s *Store) scoreSemantic(ctx context.Context, queryVec []float32, limit int) ([]scored, error) {
	// An empty query vector means "no semantic leg": degraded search (when the
	// embedding service is offline) passes a nil vector + query text to run
	// BM25-only through the fusion path. Distinct from a zero vector below,
	// which is a real embedding gone wrong and stays an error.
	if len(queryVec) == 0 {
		return nil, nil
	}
	if d := s.dim.Load(); d != 0 && int64(len(queryVec)) != d {
		return nil, fmt.Errorf("query dim %d != index dim %d", len(queryVec), d)
	}
	// Reject all-zero queries up front. vec0's cosine path would otherwise
	// produce NaN distances on a zero vector and surface nonsense rankings.
	// Done before the empty-index early-return so callers get a clear error
	// even when there's nothing to search yet.
	allZero := true
	for _, x := range queryVec {
		if x != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil, fmt.Errorf("query vector is zero")
	}
	if limit <= 0 || s.dim.Load() == 0 {
		return nil, nil
	}
	qBlob := encodeVec(queryVec)
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, distance FROM chunk_vecs
		 WHERE embedding MATCH %s AND k = ?
		 ORDER BY distance`, s.vecMatchExpr()),
		qBlob, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]scored, 0, limit)
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		// Cosine distance ∈ [0, 2]; convert to similarity ∈ [-1, 1] so
		// callers can keep the "larger = better" convention shared with
		// the BM25 leg (which flips bm25() sign for the same reason).
		out = append(out, scored{id, float32(1 - dist)})
	}
	return out, rows.Err()
}

// scoreSemanticForIDs fills in cosine similarity for a specific set of
// chunk IDs that the vec0 top-K query missed (BM25-only fused hits).
// Uses sqlite-vec's scalar vec_distance_cosine() so we can keep Hit.Score
// populated even for hits that surfaced purely through the lexical leg.
// Returns a partial map; callers must tolerate missing entries.
func (s *Store) scoreSemanticForIDs(ctx context.Context, queryVec []float32, ids []int64) (map[int64]float32, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, encodeVec(queryVec))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, vec_distance_cosine(?, vec) FROM chunks WHERE id IN (`+inPlaceholders(len(ids))+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]float32, len(ids))
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		out[id] = float32(1 - dist)
	}
	return out, rows.Err()
}

// scoreBM25 runs the FTS5 / BM25 leg of hybrid search. Returns the
// top-`limit` chunk IDs ordered by BM25 rank (best first), with the
// score field set to -bm25() (so larger = better, consistent with the
// cosine path's convention).
//
// Kind weighting: bm25() returns negative numbers (more negative =
// better). Multiplying by 0.7 for `window` chunks (free-form line
// slices, dominated by Markdown/README content) pushes them toward
// zero — i.e. worse rank — so a README that happens to list every
// identifier the codebase exposes can't crowd out the actual
// definition site. Structural chunks (function_declaration etc.) and
// `orphan` chunks (top-level const/var/import we'd lose otherwise)
// keep their full BM25 weight.
func (s *Store) scoreBM25(ctx context.Context, queryText string, limit int) ([]scored, error) {
	matchExpr := buildFTSQuery(queryText, s.opts.FTSMode)
	if matchExpr == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT chunks_fts.rowid,
		        bm25(chunks_fts, 1.0, 2.0, 0.5) * CASE chunks.kind
		            WHEN 'window' THEN 0.7
		            ELSE 1.0
		          END AS weighted_rank
		   FROM chunks_fts
		   JOIN chunks ON chunks.id = chunks_fts.rowid
		   WHERE chunks_fts MATCH ?
		   ORDER BY weighted_rank
		   LIMIT ?`,
		matchExpr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]scored, 0, limit)
	for rows.Next() {
		var id int64
		var bm float64
		if err := rows.Scan(&id, &bm); err != nil {
			return nil, err
		}
		// bm25() returns negative rank by convention (smaller = better).
		// Flip the sign so larger = better, matching cosine.
		out = append(out, scored{id, float32(-bm)})
	}
	return out, rows.Err()
}

// buildFTSQuery turns a natural-language query into an FTS5 MATCH
// expression.
//
// Tokenization mirrors the schema's `unicode61` tokenizer: anything
// that's a Unicode letter, digit, or `_` is part of an identifier.
// This keeps non-ASCII names (`ParseRFC3339Núñez`, `ユーザー認証`) from
// being silently dropped — the ASCII-only filter that used to live
// here lost those tokens entirely, so BM25 contributed nothing on
// non-ASCII queries.
//
// Quoted substrings survive as FTS5 phrases: `"package boundary"` in
// the user query becomes `"package boundary"` in the MATCH expression
// (multi-token, ordered). Useful for forcing precision on a known
// phrase even when the overall mode is OR.
//
// Join operator follows mode:
//   - Auto: AND for 1–2 terms (symbol-shaped lookup), OR for 3+
//     (natural-language question where AND would too often return zero
//     hits).
//   - AND / OR: explicit override.
func buildFTSQuery(q string, mode FTSMode) string {
	isIdentRune := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
	}
	// tokenize splits a span on non-identifier runes, dropping single-rune
	// tokens (they're noisy in BM25 and FTS5 phrases must be non-empty).
	tokenize := func(span string) []string {
		var toks []string
		var b strings.Builder
		flush := func() {
			t := b.String()
			b.Reset()
			runes := 0
			for range t {
				runes++
				if runes >= 2 {
					break
				}
			}
			if runes >= 2 {
				toks = append(toks, t)
			}
		}
		for _, r := range span {
			if isIdentRune(r) {
				b.WriteRune(r)
			} else {
				flush()
			}
		}
		flush()
		return toks
	}

	var terms []string // each is a complete FTS5 term: `"word"` or `"w1 w2"`
	runes := []rune(q)
	i := 0
	for i < len(runes) {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		if runes[i] == '"' {
			// Find closing quote; tokenize contents and emit one phrase.
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				j++
			}
			phraseToks := tokenize(string(runes[i+1 : j]))
			if len(phraseToks) > 0 {
				terms = append(terms, `"`+strings.Join(phraseToks, " ")+`"`)
			}
			i = j
			if i < len(runes) {
				i++ // step past the closing quote (or end of input)
			}
			continue
		}
		// Read until next whitespace or quote.
		start := i
		for i < len(runes) && !unicode.IsSpace(runes[i]) && runes[i] != '"' {
			i++
		}
		for _, t := range tokenize(string(runes[start:i])) {
			terms = append(terms, expandSPLADE(strings.ToLower(t), expandCamelTerm(`"`+t+`"`)))
		}
	}

	if len(terms) == 0 {
		return ""
	}
	joiner := " OR "
	switch mode {
	case FTSModeAND:
		joiner = " AND "
	case FTSModeOR:
		joiner = " OR "
	default: // Auto
		if len(terms) < 3 {
			joiner = " AND "
		}
	}
	return strings.Join(terms, joiner)
}

// scoreContext carries the per-id score maps produced by the hybrid /
// reranked search pipeline. All fields are optional (nil = not available).
type scoreContext struct {
	semCosine   map[int64]float32 // raw cosine scores from the semantic leg
	bm25Score   map[int64]float32 // BM25 scores from the FTS leg
	rrfScore    map[int64]float32 // RRF fusion scores (non-nil only on reranked path)
	rerankScore map[int64]float32 // cross-encoder scores (non-nil only on reranked path)
}

// fetchHits issues one SELECT to get content for the ranked IDs, then
// assembles Hit values with scores from sc.
//   - sc.semCosine / sc.bm25Score: nil in semantic-only mode.
//   - sc.rrfScore: non-nil on the reranked path; ranked[i].score is the
//     rerank score in that case, so RRFScore must come from the map.
//   - sc.rerankScore: non-nil on the reranked path; populates Hit.RerankScore.
func (s *Store) fetchHits(ctx context.Context, ranked []scored, sc scoreContext) ([]Hit, error) {
	if len(ranked) == 0 {
		return nil, nil
	}
	idArgs := make([]any, len(ranked))
	for i, r := range ranked {
		idArgs[i] = r.id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, kind, name, start_line, end_line, content FROM chunks WHERE id IN (`+inPlaceholders(len(idArgs))+`)`,
		idArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[int64]Hit, len(ranked))
	for rows.Next() {
		var id int64
		var h Hit
		if err := rows.Scan(&id, &h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content); err != nil {
			return nil, err
		}
		byID[id] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(ranked))
	for _, r := range ranked {
		h, ok := byID[r.id]
		if !ok {
			continue
		}
		if sc.semCosine != nil {
			h.Score = sc.semCosine[r.id]
			if sc.rrfScore != nil {
				h.RRFScore = sc.rrfScore[r.id]
			} else {
				h.RRFScore = r.score
			}
		} else {
			h.Score = r.score
		}
		if sc.bm25Score != nil {
			h.BM25Score = sc.bm25Score[r.id]
		}
		if sc.rerankScore != nil {
			h.RerankScore = sc.rerankScore[r.id]
		}
		out = append(out, h)
	}
	return out, nil
}

// FindSymbol returns chunks whose `name` column exactly matches the
// given identifier. Results are ordered by (path, start_line). Uses a
// SQL index scan — no embedding required — so it is fast regardless of
// index size.
//
// When the chunks table yields zero hits, falls back to a graph_nodes
// scan. The Go-graph layer indexes types and struct fields that don't
// produce standalone chunks (the chunker emits chunks per function/
// method/class, not per field), so a query like `MaxFileSize` finds
// the field via the graph even though chunks has nothing. Graph-fallback
// hits carry path + line range but empty Content, since graph nodes
// only point at offsets — agents can Read the range for the body.
func (s *Store) FindSymbol(ctx context.Context, name string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 10
	}
	// LEFT JOIN graph_nodes on chunk_id surfaces centrality columns for
	// the (typically single) graph node bound to each chunk. When the
	// graph hasn't been built — or the chunk isn't a function/method —
	// the COALESCEd zeros sink the row to the natural path-order tail,
	// preserving the pre-centrality default.
	//
	// Sort key: pagerank DESC, in_degree DESC, then path/line for
	// determinism on ties. Centrality is per-symbol, so two callers
	// asking "search_symbol Indexer" land on the SAME top result every
	// run, instead of whichever chunk happens to come first in path
	// order.
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.path, c.kind, c.name, c.start_line, c.end_line, c.content,
		        COALESCE(g.in_degree, 0), COALESCE(g.out_degree, 0),
		        COALESCE(g.cross_pkg_callers, 0), COALESCE(g.pagerank, 0),
		        COALESCE(g.betweenness, 0)
		 FROM chunks c
		 LEFT JOIN graph_nodes g ON g.chunk_id = c.id
		 WHERE c.name = ?
		 ORDER BY COALESCE(g.pagerank, 0) DESC,
		          COALESCE(g.in_degree, 0) DESC,
		          c.path, c.start_line
		 LIMIT ?`,
		name, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var id int64
		var h Hit
		if err := rows.Scan(&id, &h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content,
			&h.InDegree, &h.OutDegree, &h.CrossPkgCallers, &h.PageRank, &h.Betweenness); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	return s.findSymbolInGraph(ctx, name, k)
}

// findSymbolInGraph queries the Go-graph layer for nodes whose `name`
// column matches exactly. Used as a fallback by FindSymbol when the
// chunks table has nothing — covers types, struct fields, and other
// entities that don't produce standalone chunks. Returns nil on
// missing graph table (older index versions) rather than failing the
// surrounding lookup.
func (s *Store) findSymbolInGraph(ctx context.Context, name string, k int) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, name, file_path, start_line, end_line,
		        in_degree, out_degree, cross_pkg_callers, pagerank,
		        COALESCE(betweenness, 0)
		 FROM graph_nodes
		 WHERE name = ? AND file_path != '' AND start_line > 0
		 ORDER BY pagerank DESC, in_degree DESC, file_path, start_line LIMIT ?`,
		name, k)
	if err != nil {
		// graph_nodes may not exist on older indexes — degrade silently.
		return nil, nil //nolint:nilerr
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.Name, &h.Path, &h.StartLine, &h.EndLine,
			&h.InDegree, &h.OutDegree, &h.CrossPkgCallers, &h.PageRank, &h.Betweenness); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FindSymbolCandidates returns up to k distinct chunk names whose
// `name` column contains `query` as a substring. Ordered by length
// (shorter ≈ closer-in-spirit) then alphabetically. Intended as a
// "did you mean" surface for search_symbol misses — callers should pass
// the exact-name lookup query and surface the results in a hint so
// the agent can retry with a real identifier instead of guessing.
func (s *Store) FindSymbolCandidates(ctx context.Context, query string, k int) ([]string, error) {
	if k <= 0 {
		k = 5
	}
	if query == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT name FROM chunks
		 WHERE name LIKE '%' || ? || '%' AND name != '' AND name != ?
		 ORDER BY length(name), name LIMIT ?`,
		query, query, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// RelatedChunks returns the top-k chunks most similar to the chunk at
// (path, startLine), excluding the source chunk itself. Issues one vec0
// KNN query with k+1 candidates so we can drop the source (which always
// ranks first at distance 0). Returns an error if no chunk is found at
// the given location.
func (s *Store) RelatedChunks(ctx context.Context, path string, startLine int, k int) ([]Hit, error) {
	if k <= 0 {
		k = 8
	}
	var blob []byte
	var sourceID int64
	// Find the most specific chunk whose span contains startLine.
	// Exact-start match is preferred; when backfillComments shifted the
	// stored start_line to a leading doc comment, callers passing the
	// declaration line still resolve to the right chunk.
	err := s.db.QueryRowContext(ctx,
		`SELECT id, vec FROM chunks
		 WHERE path = ? AND start_line <= ? AND end_line >= ?
		 ORDER BY (end_line - start_line) ASC LIMIT 1`,
		path, startLine, startLine).Scan(&sourceID, &blob)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("no chunk at %s:%d", path, startLine)
		}
		return nil, err
	}
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("vec blob length %d not divisible by 4", len(blob))
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT rowid, distance FROM chunk_vecs
		 WHERE embedding MATCH %s AND k = ?
		 ORDER BY distance`, s.vecMatchExpr()),
		blob, k+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scores := make([]scored, 0, k)
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		if id == sourceID {
			continue
		}
		scores = append(scores, scored{id, float32(1 - dist)})
		if len(scores) >= k {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.fetchHits(ctx, scores, scoreContext{})
}

// ChunkAt returns the most specific indexed chunk whose span contains
// startLine in path. Used by search_similar to obtain the source chunk's
// content for embedding. Returns an error matching "no chunk at" when the
// location isn't indexed.
func (s *Store) ChunkAt(ctx context.Context, path string, startLine int) (Hit, error) {
	var h Hit
	err := s.db.QueryRowContext(ctx,
		`SELECT path, kind, name, start_line, end_line, content FROM chunks
		 WHERE path = ? AND start_line <= ? AND end_line >= ?
		 ORDER BY (end_line - start_line) ASC LIMIT 1`,
		path, startLine, startLine).
		Scan(&h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &h.Content)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Hit{}, fmt.Errorf("no chunk at %s:%d", path, startLine)
		}
		return Hit{}, err
	}
	return h, nil
}

// CodeFilePaths returns every real code file in the chunks table
// mapped to its inferred line count (max end_line across all its chunks).
// Used by overview to enumerate the indexed codebase without loading content.
func (s *Store) CodeFilePaths(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, MAX(end_line)
		FROM chunks
		WHERE path != ''
		GROUP BY path
		ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var p string
		var lc int
		if err := rows.Scan(&p, &lc); err != nil {
			return nil, err
		}
		out[p] = lc
	}
	return out, rows.Err()
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}
