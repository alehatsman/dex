package store

import (
	"context"
	"database/sql"
	"time"
)

// DirSummary is one LLM-generated rollup summary for a directory (package /
// subsystem / repo root). Like FileSummary it is an isolated derived artifact:
// nothing in the retrieval/fusion path reads it. Its SourceHash is a *composite*
// of its children's source hashes (see RollupHash), so it invalidates when any
// descendant file changes and is stable otherwise.
type DirSummary struct {
	Path          string // index-relative dir; "" = repo root
	SourceHash    string
	PromptVersion int
	Model         string
	Summary       string
	GeneratedAt   time.Time
}

// RollupHash derives a directory's staleness signal from its children's source
// hashes (child files' FileBodyHash + child dirs' RollupHash). It reuses the
// exact FileBodyHash construction (sort, join, sha256) so a change to any
// descendant flips this hash while untouched siblings leave it unchanged —
// giving "touch one file, re-roll only its ancestors". Empty input yields "".
func RollupHash(childHashes []string) string {
	return hashSorted(childHashes)
}

// DirSummaryMeta returns the stored source hash and prompt version for a
// directory without loading the prose — the staleness gate for the rollup pass.
func (s *Store) DirSummaryMeta(ctx context.Context, relDir string) (sourceHash string, promptVersion int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT source_hash, prompt_version FROM dir_summaries WHERE path = ?`, relDir).
		Scan(&sourceHash, &promptVersion)
	switch {
	case err == sql.ErrNoRows:
		return "", 0, false, nil
	case err != nil:
		return "", 0, false, err
	}
	return sourceHash, promptVersion, true, nil
}

// GetDirSummary returns the stored rollup summary for a directory, if present.
func (s *Store) GetDirSummary(ctx context.Context, relDir string) (DirSummary, bool, error) {
	var (
		ds  DirSummary
		gen int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT path, source_hash, prompt_version, model, summary, generated_at
		   FROM dir_summaries WHERE path = ?`, relDir).
		Scan(&ds.Path, &ds.SourceHash, &ds.PromptVersion, &ds.Model, &ds.Summary, &gen)
	switch {
	case err == sql.ErrNoRows:
		return DirSummary{}, false, nil
	case err != nil:
		return DirSummary{}, false, err
	}
	ds.GeneratedAt = time.Unix(gen, 0)
	return ds, true, nil
}

// UpsertDirSummary stores (or replaces) the rollup summary for a directory.
func (s *Store) UpsertDirSummary(ctx context.Context, ds DirSummary) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dir_summaries(path, source_hash, prompt_version, model, summary, generated_at)
		   VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   source_hash=excluded.source_hash,
		   prompt_version=excluded.prompt_version,
		   model=excluded.model,
		   summary=excluded.summary,
		   generated_at=excluded.generated_at`,
		ds.Path, ds.SourceHash, ds.PromptVersion, ds.Model, ds.Summary, ds.GeneratedAt.Unix())
	return err
}
