package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

func cmdIndexDispatch(ctx context.Context, args []string) error {
	if len(args) >= 1 {
		switch args[0] {
		case "status":
			return cmdIndexStatus(ctx, args[1:])
		}
	}
	return cmdIndex(ctx, args)
}

func cmdIndex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	setHelp(fs,
		"Build or refresh the per-project index (chunks + Go static graph).",
		"dex index [flags] <path>")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	dryRun := fs.Bool("dry-run", false, "walk the file tree and show what would be indexed, without writing to the index")
	graphMode := fs.String("graph", "on", "graph phase: on|off|only ('on' runs both phases, 'off' skips graph, 'only' skips chunk/embed and just refreshes the graph)")
	format := fs.String("format", "text", "output format: text|json")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	switch *graphMode {
	case "on", "off", "only":
	default:
		return fmt.Errorf("invalid --graph=%s (want on|off|only)", *graphMode)
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("index needs exactly one path argument")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(rest[0], base)
	if err != nil {
		return err
	}
	if err := proj.CheckIndexable(p, *force); err != nil {
		return err
	}
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	if *dryRun {
		warnIfNoInclude(ig, p.Root)
		return runIndexDryRun(ctx, p, ig, *verbose, *format)
	}
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "index", "chunk", *waitLock, *breakLock)
	if err != nil {
		return err
	}
	if lk == nil {
		return nil // another indexer is running; message already printed
	}
	defer func() { _ = lk.Release() }()
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// One embedder + vector cache shared by the chunk and graph-embed passes,
	// so a reindex reuses vectors for unchanged content instead of re-embedding
	// (#121). Built before phase 1 because --graph=only skips phase 1 but still
	// runs the graph-embed pass below.
	em, vc := indexEmbedder(p, st.EmbedModel())
	if vc != nil {
		defer func() { _ = vc.Close() }()
	}

	// Phase 1: chunk + embed (skipped when --graph=only).
	if *graphMode != "only" {
		warnIfNoInclude(ig, p.Root)
		opts := index.Options{
			Verbose:     *verbose,
			Logger:      cliLogger(),
			Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0),
			Progress:    indexProgressPrinter(*format),
		}
		ix := index.New(p, st, em, ig, opts)
		if err := ix.Run(ctx); err != nil {
			return err
		}
	}
	if err := st.SetProjectRoot(ctx, p.Root); err != nil {
		return err
	}

	// Phase 2: graph extraction (skipped when --graph=off).
	// In --graph=only mode the user explicitly asked for the graph, so a
	// failure is hard. In default mode the chunk phase has already
	// succeeded, so we warn-and-continue — losing the graph shouldn't
	// invalidate a fresh embed pass.
	var gstats *graph.Stats
	if *graphMode != "off" {
		_ = lk.SetPhase("graph")
		s, gerr := runGraphPhase(ctx, p, st, *verbose)
		if gerr != nil {
			if *graphMode == "only" {
				return gerr
			}
			fmt.Fprintf(os.Stderr, "⚠ graph phase failed: %v (chunk index is still usable)\n", gerr)
		} else {
			gstats = s
		}
	}

	// Phase 2.5: embed graph nodes (symbol KNN index).
	// Skipped when: embedder unavailable (lean/none profile) or graph was off.
	if *graphMode != "off" && gstats != nil {
		if em != nil {
			_ = lk.SetPhase("graph-embed")
			if n, err := embedGraphNodes(ctx, st, em, *verbose, cliLogger()); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed phase failed: %v\n", err)
			} else if n > 0 && *verbose {
				fmt.Printf("  [graph-embed] %d nodes embedded\n", n)
			}
		}
	}

	if *graphMode == "only" {
		// Mirror the old `graph index` output shape so existing scripts
		// piping --format=json keep parsing.
		return reportGraphStats(p.Root, gstats, *format)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	if emptyErr := emptyIncludeErr(p.Root, stats.Chunks, ig); emptyErr != nil {
		// Still emit the JSON object so machine consumers get structured
		// chunks:0, but fail the exit code either way (#161).
		if *format == "json" {
			_ = reportIndexResult(p.Root, stats, gstats)
		}
		return emptyErr
	}
	if *format == "json" {
		return reportIndexResult(p.Root, stats, gstats)
	}
	fmt.Fprintf(os.Stderr, "✓ indexed %s\n", p.Root)
	fmt.Fprintf(os.Stderr, "  chunks: %d  files: %d  dim: %d\n", stats.Chunks, stats.Files, stats.Dim)
	if gstats != nil {
		_ = reportGraphStats(p.Root, gstats, "text")
	}
	return nil
}

// runIndexDryRun walks the file tree applying all filters and prints a report
// of what would be indexed, without writing anything to the store.
func runIndexDryRun(ctx context.Context, p *proj.Project, ig *ignore.Matcher, verbose bool, format string) error {
	type fileEntry struct {
		path   string
		chunks int
	}
	type skipEntry struct {
		path   string
		reason string
	}

	var (
		included     []fileEntry
		skipped      []skipEntry
		skipIgnore   int
		skipBinary   int
		skipSecret   int
		skipSize     int
		skipMinified int
		packedDense  int
		totalChunks  int
	)

	const maxSize = int64(1 << 20) // 1 MB — mirrors index.Options default
	// Mirror the chunk-density guard so the preview matches a real run.
	guard := index.LoadChunkGuard(p.Root)

	walkErr := filepath.WalkDir(p.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(p.Root, path)
		if rel == "." {
			return nil
		}
		if ig.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			skipped = append(skipped, skipEntry{path: rel, reason: "ignored"})
			skipIgnore++
			return nil
		}
		if d.IsDir() {
			gitMarker := filepath.Join(path, ".git")
			if fi, err2 := os.Lstat(gitMarker); err2 == nil && !fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !ignore.IndexableExt(path) && !ignore.IndexableBasename(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxSize {
			skipped = append(skipped, skipEntry{path: rel, reason: "too-large"})
			skipSize++
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // symlinks already rejected above
		if err != nil {
			return nil
		}
		if ignore.LooksBinary(data) {
			skipped = append(skipped, skipEntry{path: rel, reason: "binary"})
			skipBinary++
			return nil
		}
		if !ignore.IsTestPath(rel) && ignore.LooksLikeSecret(data) {
			skipped = append(skipped, skipEntry{path: rel, reason: "secret-pattern"})
			skipSecret++
			return nil
		}
		if guard.SkipMinified && index.LooksMinified(data) {
			skipped = append(skipped, skipEntry{path: rel, reason: "minified"})
			skipMinified++
			return nil
		}
		chunks, _ := chunk.Chunks(ctx, rel, data)
		// Mirror the indexer: a file over the cap is coarsened by PackDense,
		// not dropped.
		if limit := guard.MaxChunksPerFile; limit > 0 && len(chunks) > limit {
			chunks = chunk.PackDense(rel, data, chunks)
			packedDense++
		}
		included = append(included, fileEntry{path: rel, chunks: len(chunks)})
		totalChunks += len(chunks)
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk: %w", walkErr)
	}

	totalSkipped := len(skipped)

	if format == "json" {
		type skipBreakdown struct {
			Ignored  int `json:"ignored"`
			Binary   int `json:"binary"`
			Secret   int `json:"secret_pattern"`
			TooLarge int `json:"too_large"`
			Minified int `json:"minified"`
		}
		type dryRunResult struct {
			Project     string        `json:"project"`
			DryRun      bool          `json:"dry_run"`
			Files       int           `json:"files"`
			Chunks      int           `json:"chunks"`
			PackedDense int           `json:"packed_dense"`
			Skipped     int           `json:"skipped"`
			Breakdown   skipBreakdown `json:"skip_breakdown"`
		}
		return json.NewEncoder(os.Stdout).Encode(dryRunResult{
			Project:     p.Root,
			DryRun:      true,
			Files:       len(included),
			Chunks:      totalChunks,
			PackedDense: packedDense,
			Skipped:     totalSkipped,
			Breakdown: skipBreakdown{
				Ignored:  skipIgnore,
				Binary:   skipBinary,
				Secret:   skipSecret,
				TooLarge: skipSize,
				Minified: skipMinified,
			},
		})
	}

	if verbose {
		for _, f := range included {
			fmt.Printf("  include  %-60s  %d chunks\n", f.path, f.chunks)
		}
		for _, s := range skipped {
			fmt.Printf("  skip     %-60s  %s\n", s.path, s.reason)
		}
		if len(included)+len(skipped) > 0 {
			fmt.Println()
		}
	}

	fmt.Printf("dry-run: %s\n", p.Root)
	fmt.Printf("  would index: %d files  %d chunks\n", len(included), totalChunks)
	if packedDense > 0 {
		fmt.Printf("  dense-packed: %d files (coarsened above chunk cap, not dropped)\n", packedDense)
	}

	if totalSkipped > 0 || skipIgnore > 0 {
		var parts []string
		if skipIgnore > 0 {
			parts = append(parts, fmt.Sprintf("%d ignored", skipIgnore))
		}
		if skipBinary > 0 {
			parts = append(parts, fmt.Sprintf("%d binary", skipBinary))
		}
		if skipSecret > 0 {
			parts = append(parts, fmt.Sprintf("%d secret-pattern", skipSecret))
		}
		if skipSize > 0 {
			parts = append(parts, fmt.Sprintf("%d too-large", skipSize))
		}
		if skipMinified > 0 {
			parts = append(parts, fmt.Sprintf("%d minified", skipMinified))
		}
		fmt.Printf("  skipped: %d files (%s)\n", totalSkipped, strings.Join(parts, ", "))
	}
	return nil
}

// indexResult is the JSON payload emitted by `index --format=json`
// (combined chunk + graph stats). The Graph field is omitted when
// the graph phase was skipped or failed.
type indexResult struct {
	Project string            `json:"project"`
	Chunks  int               `json:"chunks"`
	Files   int               `json:"files"`
	Dim     int               `json:"dim"`
	Graph   *graphIndexResult `json:"graph,omitempty"`
}

func reportIndexResult(project string, s store.Stats, g *graph.Stats) error {
	out := indexResult{
		Project: project,
		Chunks:  s.Chunks,
		Files:   s.Files,
		Dim:     s.Dim,
	}
	if g != nil {
		out.Graph = &graphIndexResult{
			Project:    project,
			Packages:   g.Packages,
			Nodes:      g.NodesUpserted,
			Edges:      g.EdgesUpserted,
			Pruned:     g.NodesPruned,
			PrunedEdge: g.EdgesPruned,
			Linked:     g.LinkedToChunks,
			ElapsedMS:  g.Elapsed.Milliseconds(),
			Warnings:   g.Warnings,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ─── search ────────────────────────────────────────────────────────────────

// cmdSearch dispatches `dex search <semantic|symbol>`. Mirrors
// the MCP tool names `search_semantic` / `search_symbol`.
