// Package veccache is a content-addressed, on-disk cache of embedding
// vectors keyed by an opaque string. It lets a full `dex reindex` reuse
// vectors for unchanged content instead of paying to recompute identical
// embeddings — the dominant CPU/GPU cost of indexing, and the reason a
// reindex heats the machine (#121).
//
// The cache is deliberately dumb: a key→vector sqlite table with no notion
// of what the key means. embed.CachingEmbedder owns the key recipe (model
// tag + input text) so a model swap misses cleanly; this package only
// stores, serves, and evicts. It is best-effort at every call site — a
// returned error tells the caller to fall back to a live embed, it never
// corrupts an index.
package veccache

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // register the sqlite3 driver
)

// FileName is the cache's on-disk name inside a project cache dir. It is in
// clearCacheKeepLock's keep-list so the reindex sweep preserves it (#121).
const FileName = "veccache.db"

// DefaultMaxRows bounds the cache. When the table exceeds it, the oldest-
// inserted rows are pruned. A generous default: the working set of a reindex
// is "current index contents", bounded by repo size; historical versions age
// out by insertion order.
const DefaultMaxRows = 500_000

// defaultPruneEvery is how many inserted rows trigger a periodic prune. Prune
// also runs on Open, but a long-lived process (the MCP auto-watcher opens the
// cache once and keeps it for the server's lifetime) never reopens, so without
// this the bound would only apply at startup. Overshoot above maxRows is thus
// bounded by roughly this many rows.
const defaultPruneEvery = 4096

// MaxRowsFromEnv returns the row bound from DEX_VEC_CACHE_MAX, or
// DefaultMaxRows when unset/invalid. An explicit 0 disables the bound.
func MaxRowsFromEnv() int {
	v := os.Getenv("DEX_VEC_CACHE_MAX")
	if v == "" {
		return DefaultMaxRows
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultMaxRows
	}
	return n
}

// Store is a sqlite-backed vector cache.
type Store struct {
	db         *sql.DB
	maxRows    int
	pruneEvery int          // inserted-row interval between periodic prunes
	sincePrune atomic.Int64 // rows inserted since the last prune
}

// Open opens (creating if needed) the cache at path and prunes it to maxRows.
// maxRows <= 0 disables the bound. The caller owns Close.
func Open(path string, maxRows int) (*Store, error) {
	db, err := sql.Open("sqlite3",
		"file:"+path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	// Serialize access through a single connection: the index path calls
	// Embed (hence Get/Put) sequentially, and one conn sidesteps WAL writer
	// contention entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS vec_cache (
	    key        TEXT PRIMARY KEY,
	    dim        INTEGER NOT NULL,
	    vec        BLOB    NOT NULL,
	    created_at INTEGER NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, maxRows: maxRows, pruneEvery: defaultPruneEvery}
	s.prune(context.Background()) // best-effort
	return s, nil
}

// Get returns the cached vector for each key that is present. Missing keys
// are simply absent from the returned map. A stored row whose blob length
// disagrees with its recorded dim is skipped (defensive against a truncated
// write). An error signals the caller to treat every input as a miss.
func (s *Store) Get(ctx context.Context, keys []string) (map[string][]float32, error) {
	out := make(map[string][]float32, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// Pass the key set as a single JSON-array bind param and expand it with
	// json_each: no string-built IN clause and no per-statement bind-var limit
	// to chunk around. (JSON1 is compiled into mattn/go-sqlite3 by default.)
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, dim, vec FROM vec_cache WHERE key IN (SELECT value FROM json_each(?))`,
		string(keysJSON))
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		var dim int
		var blob []byte
		if err := rows.Scan(&key, &dim, &blob); err != nil {
			return out, err
		}
		if v := decodeVec(blob); dim > 0 && len(v) == dim {
			out[key] = v
		}
	}
	return out, rows.Err()
}

// Put stores vectors for the given keys. Existing keys are left untouched
// (INSERT OR IGNORE): a key's vector is a pure function of the key, so the
// first writer wins and later writes are redundant. Best-effort at the call
// site — a returned error is non-fatal to indexing.
func (s *Store) Put(ctx context.Context, entries map[string][]float32) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO vec_cache(key, dim, vec, created_at) VALUES(?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now().UnixNano()
	for k, v := range entries {
		if len(v) == 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, k, len(v), encodeVec(v), now); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	// Periodic prune so the bound holds in a long-lived process that never
	// reopens the cache (the MCP auto-watcher). Overshoot above maxRows is
	// bounded by ~pruneEvery rows. Best-effort.
	if s.maxRows > 0 && s.pruneEvery > 0 &&
		s.sincePrune.Add(int64(len(entries))) >= int64(s.pruneEvery) {
		s.sincePrune.Store(0)
		s.prune(ctx)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// count returns the number of cached rows.
func (s *Store) count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vec_cache`).Scan(&n)
	return n, err
}

// prune deletes the oldest-inserted rows when the table exceeds maxRows.
// Best-effort: pruning is a space bound, not a correctness requirement, so
// any error is swallowed.
func (s *Store) prune(ctx context.Context) {
	if s.maxRows <= 0 {
		return
	}
	n, err := s.count(ctx)
	if err != nil || n <= s.maxRows {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM vec_cache WHERE key IN (
		    SELECT key FROM vec_cache ORDER BY created_at ASC LIMIT ?)`, n-s.maxRows)
}

// encodeVec packs a float32 slice as little-endian bytes, matching the
// store's on-disk vector encoding.
func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

// decodeVec is the inverse of encodeVec. A length not divisible by 4 yields
// nil (treated as a miss by Get).
func decodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
