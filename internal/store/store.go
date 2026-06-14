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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	metaSchemaVersion = "schema_version"
	metaIndexingAt    = "indexing_started_at"
)

// IndexingStaleAfter bounds how long an "indexing in progress" marker is
// trusted. A crashed indexer can't clear the marker (the clear runs in a
// deferred call), so a query treats a marker older than this as stale and
// reports not-indexing. Generous enough to cover a full rebuild of a large
// repo; the next index run resets the marker regardless.
const IndexingStaleAfter = 30 * time.Minute

// ErrEmbedModelMismatch is returned by EnsureEmbedModel when the active
// embedding model differs from the one previously recorded for the
// index. Two same-dim models produce vectors in different latent spaces,
// so silently mixing them would corrupt retrieval — callers must rebuild
// the index (`dex reindex <path>`) before continuing.
var ErrEmbedModelMismatch = errors.New("embedding model mismatch")

// ErrSchemaVersionMismatch is returned by migrate when the index was written
// by a binary with a different on-disk schema version. dex does not migrate
// indexes in place — the index is a disposable derived artifact, so the
// recovery path is a one-time `dex reindex` that rebuilds from source. Mirrors
// the ErrEmbedModelMismatch fail-closed contract (#431).
var ErrSchemaVersionMismatch = errors.New("schema version mismatch")

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

	// Rerank, when non-nil, is the ranking-policy hook the store delegates the
	// final rerank-and-trim step to (Search calls it over the fused candidate
	// pool). It is transport-neutral by design — the store no longer owns the
	// cross-encoder; ranking is retrieval policy, supplied by internal/retrieve
	// (#473). Nil = the store applies its own local quality rerank
	// (ApplyLocalRerank) and trims to k.
	Rerank RerankFunc

	// MaxCandidatePool caps the fused candidate pool before the rerank hook
	// runs. Zero = no cap (the natural pool size, max(5×k, 30)). The wiring
	// sets it from the reranker pool budget so the store doesn't pay to fetch
	// more candidates than the reranker will score; honored only when Rerank
	// is set.
	MaxCandidatePool int
}

// RerankFunc reorders a fused candidate pool and trims it to k. It is the
// store's ranking-policy seam (#473): the implementation lives in
// internal/retrieve (cross-encoder + cache, with a local-rerank fallback), so
// the store imports no ranking machinery. queryText is the original query;
// hits is the fused pool in pre-rerank order.
type RerankFunc func(ctx context.Context, queryText string, hits []Hit, k int) ([]Hit, error)

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

	// GraphLaneDisabled holds the graph lane out of fusion entirely —
	// FuseSpreadingActivation returns the primary hits unchanged. This is the
	// true "lane off" switch the weight cannot express: GraphLaneWeight = 0 is
	// the "unset → use default 1.0" sentinel, so it can't zero the lane. The
	// graph-sweep eval (#470) sets this to measure the lane's marginal NDCG/
	// Recall delta vs graph-off. Not env-wired; ablation/test use only.
	GraphLaneDisabled bool
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
	InfraOptions
}

type Store struct {
	db         *sql.DB
	dim        atomic.Int64 // vector dimension; set once on first upsert, read concurrently
	dimInit    sync.Mutex   // serializes first-write dim init so concurrent first UpsertMany calls don't double-init
	noVec      atomic.Bool  // true when index is BM25-only (DEX_EMBED_ENGINE=none) — no vec0 table, nil vecs
	embedModel atomic.Value // string: model identity; "" until set by EnsureEmbedModel or recovered from meta
	opts       Options      // immutable after Open

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

// SetIndexing records that a (full or incremental) re-index is underway,
// stamping the start time. Cross-process visible: a `dex serve` daemon
// reading the same DB sees the marker a separate `dex index` writes, so it
// can warn that results are partial. Pair with ClearIndexing via defer.
func (s *Store) SetIndexing(ctx context.Context, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('`+metaIndexingAt+`', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(t.UnixNano(), 10))
	return err
}

// ClearIndexing removes the in-progress marker. Called when a re-index
// finishes (success or handled error).
func (s *Store) ClearIndexing(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM meta WHERE key='`+metaIndexingAt+`'`)
	return err
}

// IndexingInProgress reports whether a re-index is currently underway,
// reading the marker live from the DB (cross-process). A marker older than
// IndexingStaleAfter is treated as a crashed indexer and reported as not
// in progress. Returns the start time when in progress.
func (s *Store) IndexingInProgress(ctx context.Context) (bool, time.Time) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='`+metaIndexingAt+`'`).Scan(&raw)
	if err != nil {
		return false, time.Time{}
	}
	nanos, perr := strconv.ParseInt(raw, 10, 64)
	if perr != nil || nanos <= 0 {
		return false, time.Time{}
	}
	started := time.Unix(0, nanos)
	if time.Since(started) > IndexingStaleAfter {
		return false, time.Time{}
	}
	return true, started
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
