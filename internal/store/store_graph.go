package store

// Graph-side persistence (graph_nodes / graph_edges). Lives in its own
// file purely for navigability; the methods are on *Store and share the
// same migrations, *sql.DB, and transactional discipline as the chunk
// side. The package-level doc lives in store.go.

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// GraphNodeRow mirrors one graph_nodes row. It exists to keep this
// package free of an import on internal/graph (which would otherwise
// be a cycle, since internal/graph depends on internal/store via the
// GraphStore interface). The graph package converts between its own
// graph.Node type and this row shape.
type GraphNodeRow struct {
	ID            string
	Kind          string
	Name          string
	QualifiedName string
	PackagePath   string
	FilePath      string
	StartLine     int
	EndLine       int
	ChunkID       int64 // 0 = NULL
	MetadataJSON  []byte
	ContentHash   string
	// Centrality columns. Populated by graph.ComputeCentrality after the
	// upsert pass. Zero on freshly-upserted rows; stays zero for nodes
	// the centrality computation skips (non-calls-graph nodes).
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	PageRank        float64
	Betweenness     float64
	CommunityID     int
}

// GraphCentralityRow is the minimal slice of GraphNodeRow needed to
// update a node's centrality columns. Used by GraphSetCentrality so
// the centrality pass doesn't have to rewrite every other field.
type GraphCentralityRow struct {
	ID              string
	InDegree        int
	OutDegree       int
	CrossPkgCallers int
	PageRank        float64
	Betweenness     float64
	CommunityID     int
}

// GraphEdgeRow mirrors one graph_edges row. Same rationale as
// GraphNodeRow.
type GraphEdgeRow struct {
	ID           string
	Kind         string
	SrcID        string
	DstID        string
	FilePath     string
	StartLine    int
	EndLine      int
	MetadataJSON []byte
	ContentHash  string
}

// ChunkLocation is the slice of chunks each graph node needs to
// resolve `chunk_id`. Returned by ChunksByPaths, consumed by the
// chunk-linkage pass in internal/graph.
type ChunkLocation struct {
	ID        int64
	Path      string
	StartLine int
	EndLine   int
}

// GraphUpsertNodes batch-upserts nodes in one transaction. Rows whose
// content_hash matches the existing row only bump last_seen_at, keeping
// the prune-by-cutoff pass correct without rewriting unchanged rows.
func (s *Store) GraphUpsertNodes(ctx context.Context, rows []GraphNodeRow, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO graph_nodes(
		   id, kind, name, qualified_name, package_path, file_path,
		   start_line, end_line, chunk_id, metadata_json, content_hash, last_seen_at
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   kind          = excluded.kind,
		   name          = excluded.name,
		   qualified_name= excluded.qualified_name,
		   package_path  = excluded.package_path,
		   file_path     = excluded.file_path,
		   start_line    = excluded.start_line,
		   end_line      = excluded.end_line,
		   chunk_id      = excluded.chunk_id,
		   metadata_json = excluded.metadata_json,
		   content_hash  = excluded.content_hash,
		   last_seen_at  = excluded.last_seen_at`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	ts := now.UnixNano()
	for _, r := range rows {
		var chunkID any
		if r.ChunkID > 0 {
			chunkID = r.ChunkID
		} else {
			chunkID = nil
		}
		if _, err := stmt.ExecContext(ctx,
			r.ID, r.Kind, r.Name, r.QualifiedName, r.PackagePath, r.FilePath,
			r.StartLine, r.EndLine, chunkID, string(r.MetadataJSON), r.ContentHash, ts); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert node %s: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

// GraphUpsertEdges batch-upserts edges in one transaction. Same
// content_hash/last_seen_at discipline as nodes.
func (s *Store) GraphUpsertEdges(ctx context.Context, rows []GraphEdgeRow, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO graph_edges(
		   id, kind, src_id, dst_id, file_path,
		   start_line, end_line, metadata_json, content_hash, last_seen_at
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   kind          = excluded.kind,
		   src_id        = excluded.src_id,
		   dst_id        = excluded.dst_id,
		   file_path     = excluded.file_path,
		   start_line    = excluded.start_line,
		   end_line      = excluded.end_line,
		   metadata_json = excluded.metadata_json,
		   content_hash  = excluded.content_hash,
		   last_seen_at  = excluded.last_seen_at`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	ts := now.UnixNano()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.ID, r.Kind, r.SrcID, r.DstID, r.FilePath,
			r.StartLine, r.EndLine, string(r.MetadataJSON), r.ContentHash, ts); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert edge %s: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

// GraphPruneUnseen deletes nodes and edges with last_seen_at strictly
// older than cutoff. Pair with a cutoff = Run start time so rows
// touched in the current pass survive.
func (s *Store) GraphPruneUnseen(ctx context.Context, cutoff time.Time) (nodes, edges int64, err error) {
	ts := cutoff.UnixNano()
	r, err := s.db.ExecContext(ctx, `DELETE FROM graph_edges WHERE last_seen_at < ?`, ts)
	if err != nil {
		return 0, 0, err
	}
	edges, _ = r.RowsAffected()
	r, err = s.db.ExecContext(ctx, `DELETE FROM graph_nodes WHERE last_seen_at < ?`, ts)
	if err != nil {
		return 0, edges, err
	}
	nodes, _ = r.RowsAffected()
	return nodes, edges, nil
}

// GraphMaxEpoch returns the maximum last_seen_at value across all graph_nodes
// and graph_edges rows, as a UnixNano int64. Returns 0 if the graph is empty.
// Used as a cheap cache-validity stamp: if the value changes, the graph view
// must be reloaded.
func (s *Store) GraphMaxEpoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(m), 0) FROM (
			SELECT MAX(last_seen_at) AS m FROM graph_nodes
			UNION ALL
			SELECT MAX(last_seen_at) AS m FROM graph_edges
		)`).Scan(&epoch)
	return epoch, err
}

// GraphStats reports the current graph_nodes / graph_edges row counts.
func (s *Store) GraphStats(ctx context.Context) (nodes, edges int64, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_nodes`).Scan(&nodes); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_edges`).Scan(&edges); err != nil {
		return nodes, 0, err
	}
	return nodes, edges, nil
}

// GraphAllNodes streams every node row. Used by exporters and tests; a
// real query API arrives in a follow-up layer.
func (s *Store) GraphAllNodes(ctx context.Context) ([]GraphNodeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, qualified_name, package_path, file_path,
		        start_line, end_line, COALESCE(chunk_id, 0), metadata_json, content_hash,
		        in_degree, out_degree, cross_pkg_callers, pagerank, betweenness,
		        COALESCE(community_id, 0)
		   FROM graph_nodes
		  ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraphNodeRow
	for rows.Next() {
		var r GraphNodeRow
		var md string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.QualifiedName, &r.PackagePath, &r.FilePath,
			&r.StartLine, &r.EndLine, &r.ChunkID, &md, &r.ContentHash,
			&r.InDegree, &r.OutDegree, &r.CrossPkgCallers, &r.PageRank, &r.Betweenness, &r.CommunityID); err != nil {
			return nil, err
		}
		r.MetadataJSON = []byte(md)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GraphSetCentrality batch-updates centrality columns on graph_nodes.
// Run after GraphUpsertNodes / GraphUpsertEdges have settled — the
// caller computes degrees + PageRank from the in-memory edge slice and
// writes the result here in a single transaction. Rows whose ID is not
// in the table are silently ignored (UPDATE is a no-op).
func (s *Store) GraphSetCentrality(ctx context.Context, rows []GraphCentralityRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`UPDATE graph_nodes
		    SET in_degree         = ?,
		        out_degree        = ?,
		        cross_pkg_callers = ?,
		        pagerank          = ?,
		        betweenness       = ?,
		        community_id      = ?
		  WHERE id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.InDegree, r.OutDegree, r.CrossPkgCallers, r.PageRank, r.Betweenness, r.CommunityID, r.ID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set centrality %s: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

// GraphAllEdges streams every edge row.
func (s *Store) GraphAllEdges(ctx context.Context) ([]GraphEdgeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, src_id, dst_id, file_path, start_line, end_line, metadata_json, content_hash
		   FROM graph_edges
		  ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraphEdgeRow
	for rows.Next() {
		var r GraphEdgeRow
		var md string
		if err := rows.Scan(&r.ID, &r.Kind, &r.SrcID, &r.DstID, &r.FilePath,
			&r.StartLine, &r.EndLine, &md, &r.ContentHash); err != nil {
			return nil, err
		}
		r.MetadataJSON = []byte(md)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChunksByPaths returns (id, start_line, end_line) for every chunk
// under any of paths. Used by the graph extractor's chunk-linkage
// pass; batched the same way as ExistingSHAsBatch to stay within
// SQLite's parameter limit.
func (s *Store) ChunksByPaths(ctx context.Context, paths []string) (map[string][]ChunkLocation, error) {
	out := make(map[string][]ChunkLocation, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	const batchSize = 500
	for i := 0; i < len(paths); i += batchSize {
		end := min(i+batchSize, len(paths))
		slice := paths[i:end]
		args := make([]any, len(slice))
		for j, p := range slice {
			args[j] = p
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, path, start_line, end_line FROM chunks WHERE path IN (`+inPlaceholders(len(slice))+`)`,
			args...)
		if err != nil {
			return nil, err
		}
		if err := scanChunkLocations(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanChunkLocations(rows *sql.Rows, out map[string][]ChunkLocation) error {
	defer rows.Close()
	for rows.Next() {
		var c ChunkLocation
		if err := rows.Scan(&c.ID, &c.Path, &c.StartLine, &c.EndLine); err != nil {
			return err
		}
		out[c.Path] = append(out[c.Path], c)
	}
	return rows.Err()
}

// GraphSymbol carries the columns the guide renderer needs to list
// exported declarations and top-centrality entry points. QualifiedName
// is preferred over Name for display because methods carry their
// receiver type there (e.g. "Store.Search" vs bare "Search").
type GraphSymbol struct {
	Name          string
	QualifiedName string
	Kind          string // function|method|struct|interface|type
	FilePath      string
	StartLine     int
	EndLine       int
	PageRank      float64
	InDegree      int
}

// dirLikePattern returns the SQL LIKE pattern that matches files
// directly under relDir (one level deep, no nested subdirs). For
// relDir="internal/store" the pattern is "internal/store/%" and the
// caller adds an additional `NOT LIKE 'internal/store/%/%'` clause.
func dirLikePattern(relDir string) string { return escapeLike(relDir) + "/%" }
func nestedExclude(relDir string) string  { return escapeLike(relDir) + "/%/%" }

// ExportedSymbolsByDir returns Go-exported declarations whose source
// file lives directly under relDir (not nested subdirectories).
// Filters to declarable kinds (function, method, struct, interface,
// type) and to capitalized names. Ordered by name. Used by the guide
// renderer to ground "Exported API" sections in real symbols instead
// of LLM-invented ones.
func (s *Store) ExportedSymbolsByDir(ctx context.Context, relDir string) ([]GraphSymbol, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, qualified_name, kind, file_path, start_line, end_line, pagerank, in_degree
		FROM graph_nodes
		WHERE file_path LIKE ? ESCAPE '\'
		  AND file_path NOT LIKE ? ESCAPE '\'
		  AND kind IN ('function','method','struct','interface','type')
		  AND substr(name,1,1) BETWEEN 'A' AND 'Z'
		ORDER BY name`,
		dirLikePattern(relDir), nestedExclude(relDir))
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// TopCentralByDir returns the top-k functions and methods under relDir
// sorted by PageRank then by in_degree. When exportedOnly is true the
// query restricts to capitalized names — useful for the renderer's
// "Key entry points" section where a reader expects the package's
// public surface, not its internal hot spots.
func (s *Store) TopCentralByDir(ctx context.Context, relDir string, k int, exportedOnly bool) ([]GraphSymbol, error) {
	visibility := ""
	if exportedOnly {
		visibility = " AND substr(name,1,1) BETWEEN 'A' AND 'Z'"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, qualified_name, kind, file_path, start_line, end_line, pagerank, in_degree
		FROM graph_nodes
		WHERE file_path LIKE ? ESCAPE '\'
		  AND file_path NOT LIKE ? ESCAPE '\'
		  AND kind IN ('function','method')`+visibility+`
		ORDER BY pagerank DESC, in_degree DESC, name ASC
		LIMIT ?`,
		dirLikePattern(relDir), nestedExclude(relDir), k)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

// SymbolsByFile returns all graph nodes whose file_path exactly matches
// relPath, ordered by start_line. Returns nil (not an error) when no
// nodes are indexed for that file.
func (s *Store) SymbolsByFile(ctx context.Context, relPath string) ([]GraphSymbol, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, qualified_name, kind, file_path, start_line, end_line, pagerank, in_degree
		FROM graph_nodes
		WHERE file_path = ?
		ORDER BY start_line`,
		relPath)
	if err != nil {
		return nil, err
	}
	return scanSymbols(rows)
}

func scanSymbols(rows *sql.Rows) ([]GraphSymbol, error) {
	defer func() { _ = rows.Close() }()
	var out []GraphSymbol
	for rows.Next() {
		var g GraphSymbol
		if err := rows.Scan(&g.Name, &g.QualifiedName, &g.Kind, &g.FilePath, &g.StartLine, &g.EndLine, &g.PageRank, &g.InDegree); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// PackageCentrality returns a per-directory PageRank sum across every
// graph node whose source file lives under that directory (any depth).
// Directories with no graph nodes are absent from the map.
//
// Used by the guide renderer to order module sections by architectural
// weight rather than alphabetically. Sum (rather than max or top-k) is
// a fair proxy: a package can earn rank by having one heavily-imported
// symbol or many moderately-used ones, and both deserve to surface
// above leaf packages.
func (s *Store) PackageCentrality(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_path, pagerank FROM graph_nodes
		WHERE file_path != '' AND pagerank > 0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]float64)
	for rows.Next() {
		var fp string
		var pr float64
		if err := rows.Scan(&fp, &pr); err != nil {
			return nil, err
		}
		// path.Dir treats forward slashes only; graph_nodes file_path is
		// always stored that way (the indexer normalizes on write).
		// Root-level files map to "." — the renderer's package_summary
		// for the root project shares that key.
		out[path.Dir(fp)] += pr
	}
	return out, rows.Err()
}

// ImportsForDir returns the unique import targets of the Go packages
// whose source files live under relDir. import nodes carry their
// owning package in `package_path` (their `file_path` is empty —
// imports are a package-level fact, not per-file), so we resolve via
// the package set rather than filtering imports by file_path.
func (s *Store) ImportsForDir(ctx context.Context, relDir string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH our_pkgs AS (
		  SELECT DISTINCT package_path FROM graph_nodes
		  WHERE file_path LIKE ? ESCAPE '\'
		    AND file_path NOT LIKE ? ESCAPE '\'
		    AND package_path != ''
		)
		SELECT DISTINCT name FROM graph_nodes
		WHERE kind = 'import'
		  AND package_path IN (SELECT package_path FROM our_pkgs)
		ORDER BY name`,
		dirLikePattern(relDir), nestedExclude(relDir))
	if err != nil {
		return nil, err
	}
	return scanStringColumn(rows)
}

// ImportsForFile returns the unique import targets of the Go package
// that owns the given source file. File-level analog of ImportsForDir.
func (s *Store) ImportsForFile(ctx context.Context, relPath string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH file_pkg AS (
		  SELECT DISTINCT package_path FROM graph_nodes
		  WHERE file_path = ?
		    AND package_path != ''
		)
		SELECT DISTINCT name FROM graph_nodes
		WHERE kind = 'import'
		  AND package_path IN (SELECT package_path FROM file_pkg)
		ORDER BY name`, relPath)
	if err != nil {
		return nil, err
	}
	return scanStringColumn(rows)
}

// UsedByPackages returns the Go package paths that import a package
// whose source files live under relDir. import nodes carry their
// importer in the package_path column (file_path is empty for them,
// since imports are a package-level fact), so the renderer-side
// caller strips the module prefix to display directories.
//
// Result excludes our_pkgs themselves so a package isn't shown as
// using itself.
func (s *Store) UsedByPackages(ctx context.Context, relDir string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH our_pkgs AS (
		  SELECT DISTINCT package_path FROM graph_nodes
		  WHERE file_path LIKE ? ESCAPE '\'
		    AND file_path NOT LIKE ? ESCAPE '\'
		    AND package_path != ''
		)
		SELECT DISTINCT package_path FROM graph_nodes
		WHERE kind = 'import'
		  AND name IN (SELECT package_path FROM our_pkgs)
		  AND package_path NOT IN (SELECT package_path FROM our_pkgs)
		  AND package_path != ''
		ORDER BY package_path`,
		dirLikePattern(relDir), nestedExclude(relDir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanStringColumn(rows)
}

func scanStringColumn(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FileCentrality returns the sum of PageRank across all graph nodes whose
// file_path equals each indexed source file. Higher = more load-bearing.
// Files with no graph nodes are absent from the map.
func (s *Store) FileCentrality(ctx context.Context) (map[string]float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_path, SUM(pagerank)
		FROM graph_nodes
		WHERE file_path != '' AND pagerank > 0
		GROUP BY file_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var path string
		var rank float64
		if err := rows.Scan(&path, &rank); err != nil {
			return nil, err
		}
		out[path] = rank
	}
	return out, rows.Err()
}

// GraphSeenTime returns the timestamp a graph Run should stamp its
// nodes/edges with — and prune by. It is max(now, latest stored
// last_seen_at + 1ns) across graph_nodes and graph_edges, so a run's
// stamp/cutoff strictly exceeds every previously stored stamp even when
// the wall clock steps backward (NTP / VM clock resync — common on WSL2).
//
// Without this, a backward step makes a later run's cutoff numerically
// smaller than the rows a prior run stamped, so GraphPruneUnseen's strict
// `last_seen_at < cutoff` comparison leaves deleted entities un-pruned
// (dex #32). Callers must read this BEFORE upserting (the upsert bumps the
// max) and use the one value for both GraphUpsertNodes/Edges and
// GraphPruneUnseen.
func (s *Store) GraphSeenTime(ctx context.Context, now time.Time) (time.Time, error) {
	var maxSeen sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT MAX(m) FROM (
			SELECT MAX(last_seen_at) AS m FROM graph_nodes
			UNION ALL
			SELECT MAX(last_seen_at) AS m FROM graph_edges
		)`).Scan(&maxSeen); err != nil {
		return time.Time{}, err
	}
	ns := now.UnixNano()
	if maxSeen.Valid && maxSeen.Int64 >= ns {
		ns = maxSeen.Int64 + 1
	}
	return time.Unix(0, ns), nil
}

// SmellSymbol is one flagged function or method in a smell report.
type SmellSymbol struct {
	QualifiedName string
	Kind          string
	FilePath      string
	StartLine     int
	EndLine       int
	Lines         int
}

// SmellFile is one flagged file in a smell report.
type SmellFile struct {
	FilePath    string
	SymbolCount int
}

// SmellReport groups all code-quality signals derived from the index.
type SmellReport struct {
	LongFunctions []SmellSymbol
	DeadExports   []SmellSymbol
	GodFiles      []SmellFile
	// GodNodes are functions/methods with extremely high in-degree or
	// cross-package caller counts — usually over-coupled utilities whose
	// signatures constrain many callers at once. Thresholds: in_degree >=
	// minGodNodeCallers OR cross_pkg_callers >= minGodNodePkgCallers.
	GodNodes []SmellSymbol
}

// Smells queries four code-quality signals directly from graph_nodes:
// long functions (body >= minFuncLines), exported symbols with no indexed
// callers (dead exports), files with many symbols (god files, >=
// minFileSymbols), and god-nodes (functions/methods with in_degree >=
// minGodNodeCallers OR cross_pkg_callers >= minGodNodePkgCallers).
// Results are capped at limit items per category.
func (s *Store) Smells(ctx context.Context, minFuncLines, minFileSymbols, minGodNodeCallers, minGodNodePkgCallers, limit int) (SmellReport, error) {
	var r SmellReport

	// Long functions.
	rows, err := s.db.QueryContext(ctx, `
		SELECT qualified_name, kind, file_path, start_line, end_line,
		       (end_line - start_line) AS lines
		FROM graph_nodes
		WHERE kind IN ('function', 'method')
		  AND file_path != ''
		  AND (end_line - start_line) >= ?
		ORDER BY (end_line - start_line) DESC
		LIMIT ?`, minFuncLines, limit)
	if err != nil {
		return r, err
	}
	if r.LongFunctions, err = scanSmellSymbols(rows); err != nil {
		return r, err
	}

	// Dead exports: exported functions/methods with no incoming calls edges.
	rows, err = s.db.QueryContext(ctx, `
		SELECT qualified_name, kind, file_path, start_line, end_line,
		       (end_line - start_line) AS lines
		FROM graph_nodes
		WHERE kind IN ('function', 'method')
		  AND file_path != ''
		  AND in_degree = 0
		  AND substr(name, 1, 1) BETWEEN 'A' AND 'Z'
		ORDER BY file_path, start_line
		LIMIT ?`, limit)
	if err != nil {
		return r, err
	}
	if r.DeadExports, err = scanSmellSymbols(rows); err != nil {
		return r, err
	}

	// God files: files with >= minFileSymbols indexed symbols.
	rows, err = s.db.QueryContext(ctx, `
		SELECT file_path, COUNT(*) AS symbol_count
		FROM graph_nodes
		WHERE kind IN ('function', 'method', 'struct', 'type', 'interface')
		  AND file_path != ''
		GROUP BY file_path
		HAVING COUNT(*) >= ?
		ORDER BY COUNT(*) DESC
		LIMIT ?`, minFileSymbols, limit)
	if err != nil {
		return r, err
	}
	if r.GodFiles, err = scanSmellFiles(rows); err != nil {
		return r, err
	}

	// God-nodes: functions/methods with very high in-degree or cross-package
	// caller counts — over-coupled symbols that constrain many callers.
	rows, err = s.db.QueryContext(ctx, `
		SELECT qualified_name, kind, file_path, start_line, end_line,
		       (end_line - start_line) AS lines
		FROM graph_nodes
		WHERE kind IN ('function', 'method')
		  AND file_path != ''
		  AND (in_degree >= ? OR cross_pkg_callers >= ?)
		ORDER BY cross_pkg_callers DESC, in_degree DESC
		LIMIT ?`, minGodNodeCallers, minGodNodePkgCallers, limit)
	if err != nil {
		return r, err
	}
	if r.GodNodes, err = scanSmellSymbols(rows); err != nil {
		return r, err
	}

	return r, nil
}

// scanSmellSymbols drains a graph_nodes query selecting the six SmellSymbol
// columns (qualified_name, kind, file_path, start_line, end_line, lines) and
// surfaces scan/iteration errors instead of swallowing them. It closes rows.
func scanSmellSymbols(rows *sql.Rows) ([]SmellSymbol, error) {
	defer rows.Close()
	var out []SmellSymbol
	for rows.Next() {
		var s SmellSymbol
		if err := rows.Scan(&s.QualifiedName, &s.Kind, &s.FilePath, &s.StartLine, &s.EndLine, &s.Lines); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanSmellFiles drains a query selecting (file_path, symbol_count) into
// SmellFile rows, surfacing scan/iteration errors. It closes rows.
func scanSmellFiles(rows *sql.Rows) ([]SmellFile, error) {
	defer rows.Close()
	var out []SmellFile
	for rows.Next() {
		var f SmellFile
		if err := rows.Scan(&f.FilePath, &f.SymbolCount); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GraphNeighborFiles returns file paths that are graph-adjacent (1-hop via any
// edge kind) to any of the seed files. Seed files are excluded from results.
// Results are ordered by PageRank so the most architecturally central
// neighbors rank first. Used by the search layer as a graph-proximity RRF lane.
func (s *Store) GraphNeighborFiles(ctx context.Context, seeds []string, limit int) ([]string, error) {
	if len(seeds) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	seedSet := make(map[string]struct{}, len(seeds))
	for _, p := range seeds {
		seedSet[p] = struct{}{}
	}
	args := make([]any, len(seeds)+1)
	for i, p := range seeds {
		args[i] = p
	}
	args[len(seeds)] = limit * 3 // over-fetch; seeds filtered client-side

	rows, err := s.db.QueryContext(ctx, `
		WITH seed_nodes AS (
		  SELECT id FROM graph_nodes WHERE file_path IN (`+inPlaceholders(len(seeds))+`)
		),
		neighbor_ids AS (
		  SELECT dst_id AS id FROM graph_edges WHERE src_id IN (SELECT id FROM seed_nodes)
		  UNION
		  SELECT src_id AS id FROM graph_edges WHERE dst_id IN (SELECT id FROM seed_nodes)
		)
		SELECT DISTINCT gn.file_path
		FROM graph_nodes gn
		JOIN neighbor_ids ni ON gn.id = ni.id
		WHERE gn.file_path != ''
		ORDER BY gn.pagerank DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		if _, isSeed := seedSet[fp]; isSeed {
			continue
		}
		out = append(out, fp)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// HitsForFiles returns chunks for the given file paths ordered by graph
// PageRank descending, so the most architecturally central chunks come first.
// Used as input for the graph-proximity RRF lane in search fusion.
func (s *Store) HitsForFiles(ctx context.Context, paths []string, k int) ([]Hit, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if k <= 0 {
		k = 30
	}
	const batchSize = 500
	var out []Hit
	for i := 0; i < len(paths) && len(out) < k; i += batchSize {
		end := min(i+batchSize, len(paths))
		slice := paths[i:end]
		want := k - len(out)
		args := make([]any, len(slice)+1)
		for j, p := range slice {
			args[j] = p
		}
		args[len(slice)] = want

		rows, err := s.db.QueryContext(ctx, `
			SELECT c.path, c.kind, c.name, c.start_line, c.end_line,
			       COALESCE(gn.pagerank, 0) AS pr, c.content
			FROM chunks c
			LEFT JOIN graph_nodes gn ON gn.chunk_id = c.id
			WHERE c.path IN (`+inPlaceholders(len(slice))+`)
			ORDER BY pr DESC
			LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		out, err = scanFileHits(rows, out)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanFileHits drains a chunks+graph_nodes query into Hit rows, appending to
// out, and surfaces scan/iteration errors instead of swallowing them. It
// closes rows.
func scanFileHits(rows *sql.Rows, out []Hit) ([]Hit, error) {
	defer rows.Close()
	for rows.Next() {
		var h Hit
		var pr float64
		if err := rows.Scan(&h.Path, &h.Kind, &h.Name, &h.StartLine, &h.EndLine, &pr, &h.Content); err != nil {
			return out, err
		}
		h.Score = float32(pr)
		out = append(out, h)
	}
	return out, rows.Err()
}

// defaultGraphGamma is the per-hop decay for the graph-proximity lane when
// Options.GraphGamma is unset. γ=0.6 makes a 1-hop neighbor (0.60) outweigh
// the old flat 0.5× lane while a 3-hop neighbor (0.22) is strongly damped —
// tuned on the retrieval eval harness (#248). Sourced from the embedded
// calibration artifact (calibration.yml / #467).
var defaultGraphGamma = CalibratedDefaults().GraphGamma

// defaultGraphLaneWeight is the flat multiplier on the graph-proximity lane
// when Options.GraphLaneWeight is unset. 1.0 = neutral (lane contribution
// equals γ^hop, ≤0.6 of a primary hit at the same rank). Raise to make
// the graph lane compete more strongly with dense+BM25 — see DEX_GRAPH_WEIGHT.
// Sourced from the embedded calibration artifact (calibration.yml / #467).
var defaultGraphLaneWeight = CalibratedDefaults().GraphLaneWeight

// fuseWithGraphNeighbors merges primary hits with graph-proximity hits via
// Reciprocal Rank Fusion (k=60). Each graph hit is weighted by
// laneWeight×γ^hop (via weightByPath, keyed on file path) so 1-hop structural
// neighbors boost more than distant ones. laneWeight scales the whole lane
// independently of hop decay — raise it to make the graph lane compete with
// dense+BM25. A path absent from weightByPath gets zero contribution rather
// than a panic.
//
// Both legs are scored purely from rank position (1/(kRRF+i+1)); the incoming
// Hit.Score magnitude is discarded. This makes the lane fusion FUSION-MODE
// INDEPENDENT: whether the primary hits arrived from FusionRRF (~1/60 scores)
// or FusionLinear ([0,1] scores), only their ORDER feeds this stage, so the
// graph lane is never "under-weighted" in linear mode and laneWeight needs no
// per-mode rescaling. Do not add an Hit.Score-magnitude term here without
// renormalizing per mode — TestFuseWithGraphNeighborsRankBasedModeInvariant
// locks this property.
func fuseWithGraphNeighbors(primary, graphHits []Hit, weightByPath map[string]float32, laneWeight float32, n int) []Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(primary)+len(graphHits))
	byKey := make(map[hitKey]Hit, len(primary)+len(graphHits))
	fromPrimary := make(map[hitKey]struct{}, len(primary))

	for i, h := range primary {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
		fromPrimary[hk] = struct{}{}
	}
	for i, h := range graphHits {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += laneWeight * weightByPath[h.Path] / float32(kRRF+i+1)
		if _, exists := byKey[hk]; !exists {
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}
	out := make([]Hit, len(all))
	for i, r := range all {
		out[i] = byKey[r.key]
		// Graph-only neighbors skip the downstream reranker (held out as a
		// breadth-only tail), so stamp the fused score they're sorted by as
		// SortScore — otherwise they'd fall back to a near-zero cosine and
		// invert the rendered score= at the tail (#518). Primary hits are
		// left alone; their SortScore comes from the rerank pass.
		if _, ok := fromPrimary[r.key]; !ok {
			out[i].SortScore = r.score
		}
	}
	return out
}

// SeedFile is a file with an initial activation weight for SpreadActivation.
type SeedFile struct {
	Path   string
	Weight float32
}

// fileEdge is one entry from fileEdgesBidirectional.
type fileEdge struct {
	srcFile string
	dstFile string
	outDeg  int // distinct neighbor files for srcFile (for fan-out normalization)
}

// fileEdgesBidirectional returns file-level edges (both directions) for the
// given source files, along with each source file's distinct out-degree.
func (s *Store) fileEdgesBidirectional(ctx context.Context, files []string) ([]fileEdge, error) {
	if len(files) == 0 {
		return nil, nil
	}
	args := make([]any, len(files))
	for i, f := range files {
		args[i] = f
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH active_nodes AS (
		  SELECT id, file_path FROM graph_nodes
		  WHERE file_path IN (`+inPlaceholders(len(files))+`) AND file_path != ''
		),
		fwd AS (
		  SELECT an.file_path AS src_file, gn.file_path AS dst_file
		  FROM graph_edges ge
		  JOIN active_nodes an ON an.id = ge.src_id
		  JOIN graph_nodes gn ON gn.id = ge.dst_id
		  WHERE gn.file_path != '' AND gn.file_path != an.file_path
		),
		rev AS (
		  SELECT an.file_path AS src_file, gn.file_path AS dst_file
		  FROM graph_edges ge
		  JOIN active_nodes an ON an.id = ge.dst_id
		  JOIN graph_nodes gn ON gn.id = ge.src_id
		  WHERE gn.file_path != '' AND gn.file_path != an.file_path
		),
		all_edges AS (
		  SELECT src_file, dst_file FROM fwd
		  UNION
		  SELECT src_file, dst_file FROM rev
		),
		out_degrees AS (
		  SELECT src_file, COUNT(*) AS out_deg FROM all_edges GROUP BY src_file
		)
		SELECT ae.src_file, ae.dst_file, od.out_deg
		FROM all_edges ae
		JOIN out_degrees od ON od.src_file = ae.src_file`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fileEdge
	for rows.Next() {
		var e fileEdge
		if err := rows.Scan(&e.srcFile, &e.dstFile, &e.outDeg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActivatedFile is one non-seed file surfaced by spreading activation, with
// its accumulated energy and the hop distance at which it was first reached
// (1-hop = direct neighbor of a seed). Hop drives the γ^hop fusion weight.
type ActivatedFile struct {
	Path   string
	Energy float32
	Hop    int
}

// defaultGraphHopCap bounds spreading-activation traversal depth. Sourced from
// the embedded calibration artifact (calibration.yml / #467).
var defaultGraphHopCap = CalibratedDefaults().GraphHopCap

// hopCap returns the configured spreading-activation depth, defaulting to
// defaultGraphHopCap when unset.
func (s *Store) hopCap() int {
	if s.opts.GraphHopCap > 0 {
		return s.opts.GraphHopCap
	}
	return defaultGraphHopCap
}

// SpreadActivation runs spreading activation over the file-level call graph
// and returns the top-n non-seed file paths by accumulated activation. It is
// a thin wrapper over spreadActivation for callers that only need the paths.
func (s *Store) SpreadActivation(ctx context.Context, seeds []SeedFile, n int) ([]string, error) {
	activated, err := s.spreadActivation(ctx, seeds, n)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(activated))
	for i, a := range activated {
		out[i] = a.Path
	}
	return out, nil
}

// spreadActivation runs spreading activation over the file-level call graph.
// Seeds carry initial activation weights (typically proportional to RRF scores).
// Energy spreads bidirectionally along graph_edges with fan-out normalization
// (each unit of energy distributes equally across all connected files) and
// per-hop decay (0.7). Pulses below threshold (1e-4) are pruned. Traversal
// stops after hopCap() iterations. Each file records the hop at which it was
// first reached. The top-n non-seed files by accumulated activation are
// returned, carrying that hop distance for γ^hop fusion weighting.
func (s *Store) spreadActivation(ctx context.Context, seeds []SeedFile, n int) ([]ActivatedFile, error) {
	const (
		decay     = float32(0.7)
		threshold = float32(1e-4)
	)
	maxHops := s.hopCap()
	if len(seeds) == 0 || n <= 0 {
		return nil, nil
	}
	seedSet := make(map[string]struct{}, len(seeds))
	activation := make(map[string]float32, len(seeds)*8)
	hopOf := make(map[string]int, len(seeds)*8)
	for _, sf := range seeds {
		seedSet[sf.Path] = struct{}{}
		activation[sf.Path] = sf.Weight
		hopOf[sf.Path] = 0
	}

	for hop := 1; hop <= maxHops; hop++ {
		var active []string
		for path, energy := range activation {
			if energy > threshold {
				active = append(active, path)
			}
		}
		if len(active) == 0 {
			break
		}
		edges, err := s.fileEdgesBidirectional(ctx, active)
		if err != nil {
			return nil, err
		}
		if len(edges) == 0 {
			break
		}
		// Spread using snapshot of current activation to ensure parallel semantics.
		snapshot := make(map[string]float32, len(activation))
		for p, e := range activation {
			snapshot[p] = e
		}
		for _, e := range edges {
			energy := snapshot[e.srcFile]
			if energy <= threshold || e.outDeg == 0 {
				continue
			}
			activation[e.dstFile] += energy * decay / float32(e.outDeg)
			// Record the first (shortest) hop at which this file lit up.
			if _, seen := hopOf[e.dstFile]; !seen {
				hopOf[e.dstFile] = hop
			}
		}
	}

	var results []ActivatedFile
	for path, energy := range activation {
		if _, isSeed := seedSet[path]; isSeed {
			continue
		}
		if energy > threshold {
			results = append(results, ActivatedFile{Path: path, Energy: energy, Hop: hopOf[path]})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Energy > results[j].Energy
	})
	if len(results) > n {
		results = results[:n]
	}
	return results, nil
}

// activationSeeds builds the spreading-activation seed set: session-recent
// files (weight 1.0) blended with their co-access neighbors (0.8×), then the
// primary semantic hits weighted proportional to score. Dedup by path, first
// write wins. Extracted from FuseSpreadingActivation to keep that method under
// the cyclomatic cap once the graph-off guard (#470) is added.
func (s *Store) activationSeeds(ctx context.Context, hits []Hit) []SeedFile {
	seeds := make([]SeedFile, 0, 16)
	seen := make(map[string]struct{}, 16)

	if ss, ok, err := s.SessionGet(ctx); err == nil && ok {
		for _, f := range ss.Files {
			if _, dup := seen[f.Path]; !dup {
				seeds = append(seeds, SeedFile{Path: f.Path, Weight: 1.0})
				seen[f.Path] = struct{}{}
			}
		}
		// Blend co-access neighbors of the session working set at 0.8×.
		// These represent files that have historically been read alongside the
		// current working set and are likely relevant even if not semantic hits.
		if sessionPaths := make([]string, 0, len(ss.Files)); len(ss.Files) > 0 {
			for _, f := range ss.Files {
				sessionPaths = append(sessionPaths, f.Path)
			}
			if neighbors, err := s.CoAccessNeighbors(ctx, sessionPaths, 8); err == nil {
				for _, p := range neighbors {
					if _, dup := seen[p]; !dup {
						seeds = append(seeds, SeedFile{Path: p, Weight: 0.8})
						seen[p] = struct{}{}
					}
				}
			}
		}
	}

	// Primary hits seed BFS (structural discovery from what's already relevant).
	var maxScore float32
	for _, h := range hits {
		if h.Score > maxScore {
			maxScore = h.Score
		}
	}
	if maxScore <= 0 {
		maxScore = 1
	}
	for _, h := range hits {
		if _, dup := seen[h.Path]; !dup {
			seeds = append(seeds, SeedFile{Path: h.Path, Weight: h.Score / maxScore})
			seen[h.Path] = struct{}{}
		}
	}
	return seeds
}

// FuseSpreadingActivation expands the hit set using spreading activation.
// When queryVec is non-nil and node_vecs is populated, seeds come from the
// top-k symbol KNN matches for the query vector (query→symbol→BFS). This
// finds structurally-coupled files regardless of whether primary semantic
// hits include the target. Falls back to primary-hit file seeds when
// node_vecs is empty or KNN returns nothing. Silently returns primary hits
// unchanged on any store failure — graph proximity is best-effort.
func (s *Store) FuseSpreadingActivation(ctx context.Context, hits []Hit, queryVec []float32, n int) []Hit {
	if len(hits) == 0 {
		return hits
	}
	// Lane held out (graph-off ablation, #470): return the primary hits
	// unchanged. This is the true "lane off" the weight can't express —
	// GraphLaneWeight = 0 is the "unset → use default 1.0" sentinel.
	if s.opts.GraphLaneDisabled {
		return hits
	}

	seeds := s.activationSeeds(ctx, hits)

	activated, err := s.spreadActivation(ctx, seeds, 15)
	if err != nil {
		return hits
	}

	// Weight each activated file by γ^hop so 1-hop structural neighbors boost
	// more than distant ones.
	gamma := s.opts.GraphGamma
	if gamma <= 0 {
		gamma = defaultGraphGamma
	}
	laneWeight := s.opts.GraphLaneWeight
	if laneWeight <= 0 {
		laneWeight = defaultGraphLaneWeight
	}

	// BFS graph hits.
	paths := make([]string, len(activated))
	weightByPath := make(map[string]float32, len(activated))
	for i, a := range activated {
		paths[i] = a.Path
		hop := a.Hop
		if hop < 1 {
			hop = 1
		}
		weightByPath[a.Path] = pow32(gamma, hop)
	}
	var graphHits []Hit
	if len(paths) > 0 {
		graphHits, err = s.HitsForFiles(ctx, paths, n*2)
		if err != nil {
			return hits
		}
	}

	// Symbol KNN structural discovery: find the closest symbol nodes to the
	// query vector and fetch chunks for files that CALL INTO those nodes.
	// Using callers (not the nodes themselves) avoids activating definition-side
	// dependencies that hurt orphan recall. For orphan queries, callers of the
	// KNN-found definition are the exact import-site targets. For structural
	// queries, callers of the KNN-found coupled file are the co-coupling targets.
	if len(queryVec) > 0 {
		if nvCount, err2 := s.NodeVecCount(ctx); err2 == nil && nvCount > 0 {
			if _, knnFiles, _, err2 := s.NodeKNN(ctx, queryVec, 3); err2 == nil {
				var knnSeedFiles []string
				for _, fp := range knnFiles {
					if fp != "" {
						knnSeedFiles = append(knnSeedFiles, fp)
					}
				}
				if callers, err2 := s.CallerFiles(ctx, knnSeedFiles, 20); err2 == nil && len(callers) > 0 {
					callerHits, err2 := s.HitsForFiles(ctx, callers, n)
					if err2 == nil {
						for _, h := range callerHits {
							if _, ok := weightByPath[h.Path]; !ok {
								// treat as 1-hop neighbor
								weightByPath[h.Path] = pow32(gamma, 1)
							}
						}
						graphHits = append(graphHits, callerHits...)
					}
				}
			}
		}
	}

	if len(graphHits) == 0 {
		return hits
	}
	return fuseWithGraphNeighbors(hits, graphHits, weightByPath, laneWeight, n)
}

// pow32 returns base^exp for a small non-negative integer exponent.
func pow32(base float32, exp int) float32 {
	out := float32(1)
	for range exp {
		out *= base
	}
	return out
}

// ─── communities ──────────────────────────────────────────────────────────

// CommunityMember is one symbol inside a community.
type CommunityMember struct {
	ID              string
	Kind            string
	QualifiedName   string
	PackagePath     string
	FilePath        string
	StartLine       int
	InDegree        int
	CrossPkgCallers int
	PageRank        float64
}

// Community is one Louvain community.
type Community struct {
	ID      int
	Members []CommunityMember
}

// GraphCommunities returns all communities with at least minMembers nodes,
// sorted by descending community size, limited to limit communities.
// Nodes within each community are sorted by descending PageRank.
func (s *Store) GraphCommunities(ctx context.Context, minMembers, limit int) ([]Community, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT community_id, id, kind, qualified_name, package_path, file_path,
		        start_line, in_degree, cross_pkg_callers, pagerank
		   FROM graph_nodes
		  WHERE community_id > 0
		    AND kind IN ('function', 'method', 'type', 'interface')
		  ORDER BY community_id, pagerank DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := map[int]*Community{}
	order := []int{} // insertion order = first-seen community IDs
	for rows.Next() {
		var cid int
		var m CommunityMember
		if err := rows.Scan(&cid, &m.ID, &m.Kind, &m.QualifiedName, &m.PackagePath, &m.FilePath,
			&m.StartLine, &m.InDegree, &m.CrossPkgCallers, &m.PageRank); err != nil {
			return nil, err
		}
		if _, ok := byID[cid]; !ok {
			byID[cid] = &Community{ID: cid}
			order = append(order, cid)
		}
		byID[cid].Members = append(byID[cid].Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Filter by minMembers, sort by descending size.
	var out []Community
	for _, cid := range order {
		c := byID[cid]
		if len(c.Members) >= minMembers {
			out = append(out, *c)
		}
	}
	sortCommunitiesBySize(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// NodeVecRow is returned by NodesNeedingEmbed.
type NodeVecRow struct {
	RowID       int64
	ID          string
	ContentHash string
	EmbedText   string
}

// nodeEmbedText formats the embed text for a graph node.
func nodeEmbedText(kind, name, qualifiedName string) string {
	if qualifiedName != "" {
		return kind + " " + qualifiedName
	}
	return kind + " " + name
}

// NodesNeedingEmbed returns up to limit graph_nodes whose content_hash
// differs from vec_hash (i.e. not yet embedded or stale).
func (s *Store) NodesNeedingEmbed(ctx context.Context, limit int) ([]NodeVecRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, id, content_hash, kind, name, qualified_name
		 FROM graph_nodes
		 WHERE vec_hash != content_hash
		 ORDER BY rowid
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeVecRow
	for rows.Next() {
		var r NodeVecRow
		var kind, name, qname string
		if err := rows.Scan(&r.RowID, &r.ID, &r.ContentHash, &kind, &name, &qname); err != nil {
			return nil, err
		}
		r.EmbedText = nodeEmbedText(kind, name, qname)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetNodeVecs stores embeddings for the given nodes, updating vec and vec_hash.
// The triggers on graph_nodes.vec sync node_vecs automatically.
func (s *Store) SetNodeVecs(ctx context.Context, rows []NodeVecRow, vecs [][]float32) error {
	for i, r := range rows {
		blob := encodeVec(vecs[i])
		if _, err := s.db.ExecContext(ctx,
			`UPDATE graph_nodes SET vec=?, vec_hash=? WHERE rowid=?`,
			blob, r.ContentHash, r.RowID); err != nil {
			return fmt.Errorf("set node vec %s: %w", r.ID, err)
		}
	}
	return nil
}

// NodeKNN queries node_vecs for the k nearest nodes to qvec and returns
// their text IDs, file paths, and cosine distances (0=identical, 1=orthogonal).
func (s *Store) NodeKNN(ctx context.Context, qvec []float32, k int) (ids, files []string, distances []float32, err error) {
	blob := encodeVec(qvec)
	rows, err := s.db.QueryContext(ctx,
		`SELECT gn.id, gn.file_path, nv.distance
		 FROM node_vecs nv
		 JOIN graph_nodes gn ON gn.rowid = nv.rowid
		 WHERE nv.embedding MATCH ? AND k = ?
		 ORDER BY nv.distance`, blob, k)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, fp string
		var dist float32
		if err := rows.Scan(&id, &fp, &dist); err != nil {
			return nil, nil, nil, err
		}
		ids = append(ids, id)
		files = append(files, fp)
		distances = append(distances, dist)
	}
	return ids, files, distances, rows.Err()
}

// CallerFiles returns up to limit distinct src_file paths from graph_edges
// whose dst_file is one of the given target files. These are the files that
// directly call or import the targets — useful for finding usage sites when
// the KNN search surfaces a definition file.
func (s *Store) CallerFiles(ctx context.Context, dstFiles []string, limit int) ([]string, error) {
	if len(dstFiles) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(dstFiles))
	args := make([]any, len(dstFiles))
	for i, f := range dstFiles {
		placeholders[i] = "?"
		args[i] = f
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT sn.file_path
		 FROM graph_edges ge
		 JOIN graph_nodes sn ON sn.id = ge.src_id
		 JOIN graph_nodes dn ON dn.id = ge.dst_id
		 WHERE dn.file_path IN (`+strings.Join(placeholders, ",")+`)
		   AND sn.file_path != ''
		 ORDER BY sn.file_path
		 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}

// NodeVecCount returns the number of rows in node_vecs.
func (s *Store) NodeVecCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_vecs`).Scan(&n)
	return n, err
}

func sortCommunitiesBySize(cs []Community) {
	for i := 1; i < len(cs); i++ {
		key := cs[i]
		j := i - 1
		for j >= 0 && len(cs[j].Members) < len(key.Members) {
			cs[j+1] = cs[j]
			j--
		}
		cs[j+1] = key
	}
}
