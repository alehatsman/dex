package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// ChunkBody is the stored text of one indexed chunk, read straight from the
// index. It lets `dex summarize` build summary input entirely from sqlite —
// no filesystem walk, no race with the indexer (#572).
type ChunkBody struct {
	StartLine   int
	EndLine     int
	ContentSHA1 string
	Content     string
}

// FileSummary is one LLM-generated per-file summary. It is an isolated derived
// artifact: nothing in the retrieval/fusion path reads it.
type FileSummary struct {
	Path          string
	SourceHash    string
	PromptVersion int
	Model         string
	Summary       string
	GeneratedAt   time.Time
}

// ChunkBodiesByPath returns every chunk body for a file, ordered by start line.
// Content is the canonical text dex already holds (chunks.content); ContentSHA1
// is the per-chunk body hash used to derive a file-level staleness signal.
func (s *Store) ChunkBodiesByPath(ctx context.Context, relPath string) ([]ChunkBody, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT start_line, end_line, content_sha1, content
		   FROM chunks WHERE path = ? ORDER BY start_line, id`, relPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkBody
	for rows.Next() {
		var b ChunkBody
		if err := rows.Scan(&b.StartLine, &b.EndLine, &b.ContentSHA1, &b.Content); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FileBodyHash derives a stable per-file source hash from a file's chunk body
// hashes. It changes when any chunk body changes but is invariant to pure
// line-shifts elsewhere in the file — unlike graph_nodes.content_hash, which is
// positional. Empty input yields "".
func FileBodyHash(bodies []ChunkBody) string {
	hs := make([]string, len(bodies))
	for i, b := range bodies {
		hs[i] = b.ContentSHA1
	}
	return hashSorted(hs)
}

// hashSorted derives a stable, order-independent hash from a set of child
// hashes: sort, join with "\n", sha256. Shared by FileBodyHash (over chunk
// body hashes) and RollupHash (over child file/dir source hashes) so the file-
// and directory-level staleness signals can never drift apart. Empty input
// yields "".
func hashSorted(hs []string) string {
	if len(hs) == 0 {
		return ""
	}
	hs = append([]string(nil), hs...)
	sort.Strings(hs)
	sum := sha256.Sum256([]byte(strings.Join(hs, "\n")))
	return hex.EncodeToString(sum[:])
}

// FileSummaryMeta returns the stored source hash and prompt version for a path
// without loading the prose — the staleness gate for `dex summarize`.
func (s *Store) FileSummaryMeta(ctx context.Context, relPath string) (sourceHash string, promptVersion int, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT source_hash, prompt_version FROM file_summaries WHERE path = ?`, relPath).
		Scan(&sourceHash, &promptVersion)
	switch {
	case err == sql.ErrNoRows:
		return "", 0, false, nil
	case err != nil:
		return "", 0, false, err
	}
	return sourceHash, promptVersion, true, nil
}

// GetFileSummary returns the stored summary for a path, if present.
func (s *Store) GetFileSummary(ctx context.Context, relPath string) (FileSummary, bool, error) {
	var (
		fs  FileSummary
		gen int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT path, source_hash, prompt_version, model, summary, generated_at
		   FROM file_summaries WHERE path = ?`, relPath).
		Scan(&fs.Path, &fs.SourceHash, &fs.PromptVersion, &fs.Model, &fs.Summary, &gen)
	switch {
	case err == sql.ErrNoRows:
		return FileSummary{}, false, nil
	case err != nil:
		return FileSummary{}, false, err
	}
	fs.GeneratedAt = time.Unix(gen, 0)
	return fs, true, nil
}

// UpsertFileSummary stores (or replaces) the summary for a path.
func (s *Store) UpsertFileSummary(ctx context.Context, fs FileSummary) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO file_summaries(path, source_hash, prompt_version, model, summary, generated_at)
		   VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   source_hash=excluded.source_hash,
		   prompt_version=excluded.prompt_version,
		   model=excluded.model,
		   summary=excluded.summary,
		   generated_at=excluded.generated_at`,
		fs.Path, fs.SourceHash, fs.PromptVersion, fs.Model, fs.Summary, fs.GeneratedAt.Unix())
	return err
}
