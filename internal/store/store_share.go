package store

import (
	"context"
	"database/sql"
	"time"
)

// ShareEntry is one cached file context in the shared cache.
type ShareEntry struct {
	ID          int64
	Path        string
	ContentHash string // SHA-256 of the raw (uncompressed) file content
	Content     string // compressed/processed content as published by the pushing agent
	PushedBy    string
	PushedAt    time.Time
	HitCount    int
}

// SharePush stores a compressed file context keyed by path. The entry is
// replaced if one already exists for the path. contentHash must be the
// SHA-256 hex of the raw file so pull callers can detect staleness.
func (s *Store) SharePush(ctx context.Context, path, contentHash, content, pushedBy string) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO share_cache(path, content_hash, content, pushed_by, pushed_at, hit_count)
		   VALUES(?,?,?,?,?,0)
		   ON CONFLICT(path) DO UPDATE SET
		     content_hash=excluded.content_hash,
		     content=excluded.content,
		     pushed_by=excluded.pushed_by,
		     pushed_at=excluded.pushed_at,
		     hit_count=0`,
		path, contentHash, content, pushedBy, now)
	return err
}

// SharePull retrieves the cached content for path if currentHash matches
// the stored content_hash. Returns (content, hitCount, true, nil) on a
// cache hit. Returns ("", 0, false, nil) when the entry is missing or
// stale (hash mismatch — the caller should re-read the file and re-push).
// Stale entries are deleted automatically.
func (s *Store) SharePull(ctx context.Context, path, currentHash string) (string, int, bool, error) {
	var entry ShareEntry
	var tsNs int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, content_hash, content, pushed_by, pushed_at, hit_count
		   FROM share_cache WHERE path=?`, path).
		Scan(&entry.ID, &entry.ContentHash, &entry.Content, &entry.PushedBy, &tsNs, &entry.HitCount)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	entry.PushedAt = time.Unix(0, tsNs)

	if entry.ContentHash != currentHash {
		// stale — evict and signal miss
		_, _ = s.db.ExecContext(ctx, `DELETE FROM share_cache WHERE id=?`, entry.ID)
		return "", 0, false, nil
	}

	// cache hit — bump hit_count
	_, _ = s.db.ExecContext(ctx,
		`UPDATE share_cache SET hit_count=hit_count+1 WHERE id=?`, entry.ID)
	return entry.Content, entry.HitCount + 1, true, nil
}

// ShareList returns all entries in the shared cache.
func (s *Store) ShareList(ctx context.Context) ([]ShareEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, content_hash, pushed_by, pushed_at, hit_count
		   FROM share_cache ORDER BY pushed_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ShareEntry
	for rows.Next() {
		var e ShareEntry
		var tsNs int64
		if err := rows.Scan(&e.ID, &e.Path, &e.ContentHash, &e.PushedBy, &tsNs, &e.HitCount); err != nil {
			return nil, err
		}
		e.PushedAt = time.Unix(0, tsNs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ShareClear removes all entries from the shared cache.
func (s *Store) ShareClear(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM share_cache`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
