package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func cmdIndexStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index status", flag.ContinueOnError)
	setHelp(fs,
		"Show endpoint health and project stats (chunks/files/graph). Optional path narrows to one project. (MCP: status, DEX_EXPERT)",
		"dex index status [<path>]")
	format := fs.String("format", "text", "output format: text|json")
	jsonFlag := fs.Bool("json", false, "shorthand for --format=json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if *jsonFlag {
		*format = "json"
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	base, err := indexDir()
	if err != nil {
		return err
	}

	if len(rest) == 1 {
		return statusOneProject(ctx, checkCtx, base, rest[0], *format)
	}
	return statusAllProjects(ctx, checkCtx, base, *format)
}

// statusOneProject renders endpoint health and stats for a single project at
// path, in text or json. A missing index reports "not-indexed" rather than an
// error so the command stays informational.
func statusOneProject(ctx, checkCtx context.Context, base, path, format string) error {
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if format == "json" {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"project": p.Root, "status": "not-indexed"})
			}
			fmt.Printf("\n%s\n  not indexed — run: dex index %s\n", p.Root, p.Root)
			return nil
		}
		return err
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	nodes, edges, _ := st.GraphStats(ctx)
	stale := isStaleEmbed(stats.EmbedModel)
	if format == "json" {
		out := map[string]any{
			"project":    p.Root,
			"status":     "ok",
			"files":      stats.Files,
			"chunks":     stats.Chunks,
			"dim":        stats.Dim,
			"nodes":      nodes,
			"edges":      edges,
			"last_index": stats.LastIndex,
		}
		if stale {
			out["stale"] = true
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	// Header line up top groups version + index dir so the rest of
	// the output reads as content under a single banner instead of
	// orphaned bits between sections.
	fmt.Printf("dex %s · %s\n\n", mcp.Version, base)
	printEndpoints(checkCtx)
	fmt.Println()
	fmt.Printf("  %s\n", p.Root)
	printProjectStatLines("    ", projectStats{
		lastIndex: stats.LastIndex,
		files:     stats.Files,
		chunks:    stats.Chunks,
		nodes:     nodes,
		edges:     edges,
		dim:       stats.Dim,
		stale:     stale,
	})
	// Action hints only on the per-project view — the
	// multi-project listing keeps the per-block content uniform.
	if stale {
		fmt.Printf("    → embed model changed — run: dex reindex %s\n", p.Root)
	}
	if !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
		fmt.Printf("    → stale — run: dex index %s\n", p.Root)
	}
	return nil
}

// statusRow is one project's line in the all-project summary. corrupt/
// staleSchema/empty are mutually-exclusive health flags set by scanProjectRow.
type statusRow struct {
	root        string
	cacheHash   string // first 5 chars of cache dir name; used for untagged display
	chunks      int
	files       int
	nodes       int64
	edges       int64
	last        time.Time
	stale       bool
	staleSchema bool // index built before/under a different schema version — reindex, not damaged
	corrupt     bool
	empty       bool
}

// scanProjectRow opens the index in cache dir name (at dbPath) and reads its
// stats into a statusRow. A schema-version mismatch is recoverable (staleSchema,
// not corrupt, #489); a genuinely unreadable database is corrupt; a zero-chunk
// index is empty.
func scanProjectRow(ctx context.Context, name, dbPath string) statusRow {
	// openStore (not store.Open) so DEX_VECTOR_QUANT is honored — a
	// default-Options open resolves quant to float32 and would make this
	// read-only listing drop+rebuild chunk_vecs on an int8 index (#334).
	st, err := openStore(ctx, dbPath)
	if err != nil {
		// A schema-version mismatch is not damage — the db was written by an
		// older binary (or one before the schema-version mechanism, so
		// meta.schema_version is empty). The data is intact; the user just needs
		// `dex reindex`. Reserve "corrupt" for genuinely unreadable dbs (#489).
		if errors.Is(err, store.ErrSchemaVersionMismatch) {
			return statusRow{root: fmt.Sprintf("(schema mismatch: %s)", name), staleSchema: true}
		}
		return statusRow{root: fmt.Sprintf("(corrupt cache: %s)", name), corrupt: true}
	}
	stats, _ := st.Stats(ctx)
	root, _ := st.ProjectRoot(ctx)
	nodes, edges, _ := st.GraphStats(ctx)
	st.Close()
	if stats.Chunks == 0 {
		return statusRow{empty: true}
	}
	cacheHash := name
	if len(cacheHash) > 5 {
		cacheHash = cacheHash[:5]
	}
	if root == "" {
		root = fmt.Sprintf("untagged (%s…)", cacheHash)
	}
	return statusRow{
		root:      root,
		cacheHash: cacheHash,
		chunks:    stats.Chunks,
		files:     stats.Files,
		nodes:     nodes,
		edges:     edges,
		last:      stats.LastIndex,
		stale:     isStaleEmbed(stats.EmbedModel),
	}
}

// statusAllProjects renders endpoint health (text mode) and a recency-sorted
// summary of every indexed project under base, in text or json.
func statusAllProjects(ctx, checkCtx context.Context, base, format string) error {
	if format == "text" {
		// Header only in text mode
		fmt.Printf("dex %s · %s\n\n", mcp.Version, base)
		printEndpoints(checkCtx)
		fmt.Println()
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("\nindex dir: %s\nno projects indexed yet\n", base)
			return nil
		}
		return err
	}

	results := make([]statusRow, len(entries))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(base, e.Name(), "index.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		wg.Add(1)
		go func(idx int, name, path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = scanProjectRow(ctx, name, path)
		}(i, e.Name(), dbPath)
	}
	wg.Wait()

	var rows []statusRow
	var empties int
	for _, r := range results {
		switch {
		case r.empty:
			empties++
		case r.root != "":
			rows = append(rows, r)
		}
	}

	if len(rows) == 0 && empties == 0 {
		if format == "json" {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"projects": []any{}})
		}
		fmt.Println("projects (0 indexed)")
		fmt.Println("  no projects indexed yet — run: dex index <path>")
		return nil
	}

	// Sort by recency descending. Zero timestamps sink to the bottom
	// so genuinely-stale and unidentifiable indexes don't fight the
	// fresh ones for screen space.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].last.IsZero() != rows[j].last.IsZero() {
			return !rows[i].last.IsZero()
		}
		return rows[i].last.After(rows[j].last)
	})

	if format == "json" {
		return renderAllProjectsJSON(rows)
	}
	renderAllProjectsText(rows, empties)
	return nil
}

// renderAllProjectsJSON emits the {"projects":[…]} array, surfacing the
// reindex remedy for schema-mismatched indexes rather than marking them damaged.
func renderAllProjectsJSON(rows []statusRow) error {
	type jsonRow struct {
		Project        string    `json:"project"`
		Files          int       `json:"files"`
		Chunks         int       `json:"chunks"`
		Nodes          int64     `json:"nodes"`
		Edges          int64     `json:"edges"`
		LastIndex      time.Time `json:"last_index"`
		Corrupt        bool      `json:"corrupt,omitempty"`
		Stale          bool      `json:"stale,omitempty"`
		SchemaMismatch bool      `json:"schema_mismatch,omitempty"`
		Remedy         string    `json:"remedy,omitempty"`
	}
	out := make([]jsonRow, 0, len(rows))
	for _, r := range rows {
		jr := jsonRow{
			Project:        r.root,
			Files:          r.files,
			Chunks:         r.chunks,
			Nodes:          r.nodes,
			Edges:          r.edges,
			LastIndex:      r.last,
			Corrupt:        r.corrupt,
			Stale:          r.stale,
			SchemaMismatch: r.staleSchema,
		}
		// A schema mismatch is recoverable — surface the remedy
		// rather than treating the index as damaged (#489).
		if r.staleSchema {
			jr.Remedy = "run dex reindex"
		}
		out = append(out, jr)
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"projects": out})
}

// renderAllProjectsText prints the stacked per-project blocks with a header
// that counts healthy / stale-schema / corrupt indexes separately (#489).
func renderAllProjectsText(rows []statusRow, empties int) {
	var healthy, staleSchema, corrupt int
	for _, r := range rows {
		switch {
		case r.corrupt:
			corrupt++
		case r.staleSchema:
			staleSchema++
		default:
			healthy++
		}
	}
	header := fmt.Sprintf("projects (%d indexed", healthy)
	if staleSchema > 0 {
		header += fmt.Sprintf(", %d stale", staleSchema)
	}
	if corrupt > 0 {
		header += fmt.Sprintf(", %d corrupt", corrupt)
	}
	fmt.Println(header + ")")

	// Stacked layout: each project is a self-contained block of
	// labelled key:value rows. The labels are padded to a fixed width
	// so values line up vertically inside each block. We don't try to
	// align the value column ACROSS blocks — different projects have
	// different stat counts and aligning across them gives up
	// scannability inside a single block for nothing useful.
	for i, r := range rows {
		if i > 0 {
			fmt.Println()
		}
		if r.corrupt {
			fmt.Printf("  %s\n    CORRUPT\n", r.root)
			continue
		}
		if r.staleSchema {
			fmt.Printf("  %s\n    stale (schema mismatch) — run: dex reindex\n", r.root)
			continue
		}
		fmt.Printf("  %s\n", r.root)
		printProjectStatLines("    ", projectStats{
			lastIndex: r.last,
			files:     r.files,
			chunks:    r.chunks,
			nodes:     r.nodes,
			edges:     r.edges,
			stale:     r.stale,
		})
	}
	if empties > 0 {
		noun := "index"
		if empties != 1 {
			noun = "indexes"
		}
		fmt.Printf("\n  (%d empty %s skipped)\n", empties, noun)
	}
}

// ─── nuke ──────────────────────────────────────────────────────────────────
