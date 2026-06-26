package store

// Graph-side persistence (graph_nodes / graph_edges). Lives in its own
// file purely for navigability; the methods are on *Store and share the
// same migrations, *sql.DB, and transactional discipline as the chunk
// side. The package-level doc lives in store.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
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
	// Refactor-target columns (#591). Signature is the declaration header;
	// StartByte/EndByte are the symbol's 0-based, end-exclusive byte span in
	// the source file; DeclarationHash hashes the signature (distinct from
	// the positional ContentHash). Populated by the Go extractor for
	// function/method/type nodes and by the tree-sitter extractors for byte
	// spans; zero/empty elsewhere.
	Signature       string
	StartByte       int
	EndByte         int
	DeclarationHash string
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
		   start_line, end_line, chunk_id, metadata_json, content_hash, last_seen_at,
		   signature, start_byte, end_byte, declaration_hash
		 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   kind            = excluded.kind,
		   name            = excluded.name,
		   qualified_name  = excluded.qualified_name,
		   package_path    = excluded.package_path,
		   file_path       = excluded.file_path,
		   start_line      = excluded.start_line,
		   end_line        = excluded.end_line,
		   chunk_id        = excluded.chunk_id,
		   metadata_json   = excluded.metadata_json,
		   content_hash    = excluded.content_hash,
		   last_seen_at    = excluded.last_seen_at,
		   signature       = excluded.signature,
		   start_byte      = excluded.start_byte,
		   end_byte        = excluded.end_byte,
		   declaration_hash= excluded.declaration_hash`)
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
			r.StartLine, r.EndLine, chunkID, string(r.MetadataJSON), r.ContentHash, ts,
			r.Signature, r.StartByte, r.EndByte, r.DeclarationHash); err != nil {
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
// touched in the current pass survive. Both DELETEs run in a single
// transaction so a crash between them cannot leave orphaned nodes.
func (s *Store) GraphPruneUnseen(ctx context.Context, cutoff time.Time) (nodes, edges int64, err error) {
	ts := cutoff.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM graph_edges WHERE last_seen_at < ?`, ts)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	edges, _ = r.RowsAffected()
	r, err = tx.ExecContext(ctx, `DELETE FROM graph_nodes WHERE last_seen_at < ?`, ts)
	if err != nil {
		_ = tx.Rollback()
		return 0, edges, err
	}
	nodes, _ = r.RowsAffected()
	return nodes, edges, tx.Commit()
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
		        COALESCE(community_id, 0),
		        signature, start_byte, end_byte, declaration_hash
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
			&r.InDegree, &r.OutDegree, &r.CrossPkgCallers, &r.PageRank, &r.Betweenness, &r.CommunityID,
			&r.Signature, &r.StartByte, &r.EndByte, &r.DeclarationHash); err != nil {
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

// ExternalImports returns the distinct third-party/stdlib import paths the
// project depends on: `import` nodes whose path is NOT one of the project's own
// package paths (the same internal-vs-external criterion the graphquery View
// uses — NodesByPackage keyed on package_path). Sorted, so an orientation
// render built from it stays byte-stable / cache-friendly (#581).
func (s *Store) ExternalImports(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT qualified_name FROM graph_nodes
		WHERE kind = 'import'
		  AND qualified_name != ''
		  AND qualified_name NOT IN (
		    SELECT DISTINCT package_path FROM graph_nodes WHERE package_path != ''
		  )
		ORDER BY qualified_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PackageImport is one internal package→package dependency edge: `From`
// imports `To`, both project packages (#581 layers).
type PackageImport struct {
	From string
	To   string
}

// InternalPackageImports returns the project's internal package→package import
// edges — `import` nodes whose imported path (qualified_name) is itself a
// project package, paired with the importing package (package_path). External
// imports and self-imports are excluded. Powers the orientation "layers"
// section: a Kahn topo-sort of these edges into dependency layers (#581). Go
// forbids import cycles so the graph is a DAG; tree-sitter langs may differ, so
// the layerizer guards against cycles.
func (s *Store) InternalPackageImports(ctx context.Context) ([]PackageImport, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT package_path AS importer, qualified_name AS imported
		FROM graph_nodes
		WHERE kind = 'import'
		  AND package_path != '' AND qualified_name != ''
		  AND qualified_name != package_path
		  AND package_path NOT LIKE '%testdata%' AND qualified_name NOT LIKE '%testdata%'
		  AND package_path NOT LIKE '%/vendor/%' AND qualified_name NOT LIKE '%/vendor/%'
		  AND qualified_name IN (
		    SELECT DISTINCT package_path FROM graph_nodes
		    WHERE kind='package' AND package_path != ''
		      AND package_path NOT LIKE '%testdata%' AND package_path NOT LIKE '%/vendor/%'
		  )
		ORDER BY importer, imported`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PackageImport
	for rows.Next() {
		var e PackageImport
		if err := rows.Scan(&e.From, &e.To); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MainEntrypoints returns the file paths of the project's `main` functions —
// where execution starts. Sorted (byte-stable for the orientation render) and
// deduped. Empty for a library with no main (the orient section is then
// omitted). Used by the orientation "entrypoints" section (#581).
func (s *Store) MainEntrypoints(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT file_path FROM graph_nodes
		WHERE kind = 'function' AND name = 'main' AND file_path != ''
		  AND file_path NOT LIKE '%/testdata/%' AND file_path NOT LIKE 'testdata/%'
		  AND file_path NOT LIKE '%/vendor/%' AND file_path NOT LIKE 'vendor/%'
		ORDER BY file_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GraphScale is a one-glance size summary of the indexed graph (#581 "scale"
// orientation section): how many source files, project packages, declared
// symbols, and call edges the repo holds. Counts exclude testdata/vendor so the
// number reflects the project a reader is orienting in, not its fixtures.
type GraphScale struct {
	Files     int // source files (kind='file' nodes)
	Packages  int // project packages (kind='package' nodes)
	Symbols   int // declarations: function|method|struct|interface|type|class
	CallEdges int // 'calls' edges — the call-graph density
}

// Empty reports whether the scale carries no signal (unindexed / graph-less),
// so the orientation render can omit the section.
func (g GraphScale) Empty() bool {
	return g.Files == 0 && g.Packages == 0 && g.Symbols == 0 && g.CallEdges == 0
}

// GraphScale returns the project's size counts for the orientation "scale"
// section. One round-trip via scalar subqueries; deterministic (pure counts), so
// the rendered line stays byte-stable / cache-friendly. testdata and vendor are
// excluded by the same path criteria the sibling orient queries use.
func (s *Store) GraphScale(ctx context.Context) (GraphScale, error) {
	const notFixture = `
		AND file_path NOT LIKE '%/testdata/%' AND file_path NOT LIKE 'testdata/%'
		AND file_path NOT LIKE '%/vendor/%' AND file_path NOT LIKE 'vendor/%'`
	var g GraphScale
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM graph_nodes WHERE kind='file' AND file_path != ''`+notFixture+`),
		  (SELECT count(*) FROM graph_nodes WHERE kind='package' AND package_path != ''
		     AND package_path NOT LIKE '%testdata%' AND package_path NOT LIKE '%/vendor/%'),
		  (SELECT count(*) FROM graph_nodes
		     WHERE kind IN ('function','method','struct','interface','type','class')`+notFixture+`),
		  (SELECT count(*) FROM graph_edges WHERE kind='calls')`).
		Scan(&g.Files, &g.Packages, &g.Symbols, &g.CallEdges)
	if err != nil {
		return GraphScale{}, err
	}
	return g, nil
}

// SymbolEditSpan is the precise edit target for a graph node (#591): the
// source file plus the symbol's byte span and declaration signature. A
// refactor consumer resolves a node to this and applies a byte-range edit
// (source[StartByte:EndByte]) without reading the whole file first.
// StartByte/EndByte are 0-based and end-exclusive; they are 0 for nodes the
// extractor did not populate (e.g. package/import nodes), so callers should
// treat StartByte==EndByte==0 as "no span".
type SymbolEditSpan struct {
	ID              string
	QualifiedName   string
	Kind            string
	FilePath        string
	StartLine       int
	EndLine         int
	StartByte       int
	EndByte         int
	Signature       string
	DeclarationHash string
}

const symbolEditSpanCols = `id, qualified_name, kind, file_path,
	        start_line, end_line, start_byte, end_byte, signature, declaration_hash`

// SymbolEditSpanByID resolves a graph node ID to its edit span. ok is false
// (with a nil error) when no node carries that ID.
func (s *Store) SymbolEditSpanByID(ctx context.Context, id string) (SymbolEditSpan, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+symbolEditSpanCols+` FROM graph_nodes WHERE id = ?`, id)
	span, err := scanEditSpanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SymbolEditSpan{}, false, nil
	}
	if err != nil {
		return SymbolEditSpan{}, false, err
	}
	return span, true, nil
}

// SymbolEditSpansByName resolves a qualified name to every matching node's
// edit span, ordered by file then start line. A bare name can be defined in
// several packages, so this returns all interpretations; the caller
// disambiguates (e.g. by package) exactly like the call-graph lanes do.
func (s *Store) SymbolEditSpansByName(ctx context.Context, qualifiedName string) ([]SymbolEditSpan, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+symbolEditSpanCols+`
		   FROM graph_nodes
		  WHERE qualified_name = ?
		  ORDER BY file_path, start_line`, qualifiedName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SymbolEditSpan
	for rows.Next() {
		span, err := scanEditSpan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, span)
	}
	return out, rows.Err()
}

// editSpanScanner is the row/rows-agnostic Scan surface shared by the two
// SymbolEditSpan queries (database/sql's *Row and *Rows both satisfy it).
type editSpanScanner interface {
	Scan(dest ...any) error
}

func scanEditSpan(sc editSpanScanner) (SymbolEditSpan, error) {
	var e SymbolEditSpan
	err := sc.Scan(&e.ID, &e.QualifiedName, &e.Kind, &e.FilePath,
		&e.StartLine, &e.EndLine, &e.StartByte, &e.EndByte, &e.Signature, &e.DeclarationHash)
	return e, err
}

func scanEditSpanRow(row *sql.Row) (SymbolEditSpan, error) { return scanEditSpan(row) }

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
