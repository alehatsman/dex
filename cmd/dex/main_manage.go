package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/logx"
	"github.com/alehatsman/dex/internal/proj"
)

func cmdNuke(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("nuke", flag.ContinueOnError)
	setHelp(fs,
		"Delete the on-disk index for a project (irreversible). Prompts on a TTY; non-interactive callers must pass --yes.",
		"dex nuke [--yes] <path>")
	yes := fs.Bool("yes", false, "skip the interactive prompt (required when stdin is not a terminal)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("nuke needs exactly one path argument")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(rest[0], base)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Path is gone — compute the cache key directly from the supplied
		// string (no realpath resolution possible) and fall through.
		p, err = proj.ResolveDeleted(rest[0], base)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(p.CacheDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("nothing to remove: no index for %s\n", p.Root)
			return nil
		}
		return err
	}
	if !*yes {
		if !stdinIsTTY() {
			return fmt.Errorf("refusing to nuke without --yes: stdin is not a terminal (would be silent in scripts)")
		}
		fmt.Fprintf(os.Stderr, "About to delete index for %s\n  cache: %s\nThis is irreversible. Continue? [y/N] ", p.Root, p.CacheDir)
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(strings.ToLower(line))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}
	if err := os.RemoveAll(p.CacheDir); err != nil {
		return err
	}
	fmt.Printf("✓ removed index for %s\n", p.Root)
	return nil
}

// stdinIsTTY reports whether stdin is a character device (terminal).
// Used to gate interactive prompts so scripted invocations don't hang.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// stdoutIsTTY reports whether stdout is a character device (terminal).
// Used to gate answer streaming so piped/redirected output stays one-shot.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ─── reindex ───────────────────────────────────────────────────────────────

func cmdReindex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	setHelp(fs,
		"Drop and re-embed a project from scratch (or every known project with --all --yes).",
		"dex reindex [flags] <path>  |  dex reindex --all --yes")
	all := fs.Bool("all", false, "drop and re-index every known project under DEX_INDEX_DIR")
	yes := fs.Bool("yes", false, "confirm the destructive sweep required by --all")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	pullModel := fs.Bool("pull-model", false, "pull the default ollama embedding model (qwen3-embedding:4b) before reindexing")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	rest := fs.Args()

	if *pullModel {
		model := embed.DefaultPullModel
		fmt.Printf("pulling ollama model %q …\n", model)
		if err := embed.PullOllamaModel(ctx, model, os.Stdout); err != nil {
			return fmt.Errorf("pull model: %w", err)
		}
		fmt.Printf("pulled %q — continuing with reindex\n", model)
	}

	if *all {
		if len(rest) != 0 {
			return fmt.Errorf("reindex --all takes no path argument")
		}
		if !*yes {
			return fmt.Errorf("reindex --all drops every project index and re-embeds from scratch; pass --yes to confirm")
		}
		roots, err := proj.KnownRoots(ctx, base, proj.WarnStderr)
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			fmt.Printf("nothing to reindex under %s\n", base)
			return nil
		}
		var failed []string
		for _, root := range roots {
			fmt.Printf("→ reindexing %s\n", root)
			if err := reindexOne(ctx, root, base, *verbose, *force, *waitLock, *breakLock); err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %v\n", err)
				failed = append(failed, root)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d project(s) failed to reindex", len(failed), len(roots))
		}
		return nil
	}

	if len(rest) != 1 {
		return fmt.Errorf("reindex needs exactly one path argument (or --all)")
	}
	return reindexOne(ctx, rest[0], base, *verbose, *force, *waitLock, *breakLock)
}

// reindexOne drops the existing per-project cache dir and re-runs the
// indexer from scratch. Used by both `reindex <path>` and the loop in
// `reindex --all`.
func reindexOne(ctx context.Context, root, base string, verbose, force, waitLock, breakLock bool) error {
	p, err := proj.Resolve(root, base)
	if err != nil {
		return err
	}
	if err := proj.CheckIndexable(p, force); err != nil {
		return err
	}
	// Ensure the cache dir exists before acquiring the lock — the lock
	// file lives inside it. The destructive sweep below preserves the
	// lockfile so the holder fd stays valid.
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "reindex", "chunk", waitLock, breakLock)
	if err != nil {
		return err
	}
	if lk == nil {
		return nil // another indexer is running; message already printed
	}
	defer func() { _ = lk.Release() }()
	// Read the embed model recorded in the existing index before clearing it.
	// Preserved as the default so a plain `dex reindex` (no DEX_EMBED_MODEL)
	// stays consistent with the original build and won't produce a dim mismatch.
	var priorEmbedModel string
	if prior, err := openStore(ctx, p.DBPath); err == nil {
		priorEmbedModel = prior.EmbedModel()
		_ = prior.Close()
	}
	// Build into a temp DB adjacent to the real one. On success we clear the
	// old cache and rename the temp into place atomically. On failure the temp
	// is removed and the original index.db is left untouched, so a mid-run
	// embed outage no longer leaves the project with zero chunks (#715).
	newDBPath := p.DBPath + ".new"
	_ = os.Remove(newDBPath) // clean up any leftover from a previous failed run
	st, err := openStore(ctx, newDBPath)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = st.Close()
		if !committed {
			_ = os.Remove(newDBPath)
		}
	}()
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	warnIfNoInclude(ig, p.Root)
	ixOpts := index.Options{Verbose: verbose, Logger: cliLogger(), Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0)}
	// One embedder + vector cache shared by the chunk and graph-embed passes.
	// The cache survives the clearCacheKeepLock sweep below, so this reindex
	// reuses vectors for unchanged content instead of re-embedding (#121).
	em, vc := indexEmbedder(p, priorEmbedModel)
	if vc != nil {
		defer func() { _ = vc.Close() }()
	}
	ix := index.New(p, st, em, ig, ixOpts)
	if err := ix.Run(ctx); err != nil {
		return err
	}
	if err := st.SetProjectRoot(ctx, p.Root); err != nil {
		return err
	}
	_ = lk.SetPhase("graph")
	gstats, gerr := runGraphPhase(ctx, p, st, verbose)
	if gerr != nil {
		fmt.Fprintf(os.Stderr, "⚠ graph phase failed for %s: %v (chunk index is still usable)\n", p.Root, gerr)
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	if gstats != nil {
		if em != nil {
			if _, err := embedGraphNodes(ctx, st, em, false, cliLogger()); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed failed for %s: %v\n", p.Root, err)
			}
		}
	}

	// All work succeeded. Close the temp store, swap it into place, then
	// wipe old ephemeral files (WAL, SHM, chunk vectors, etc.).
	// Rename first so the new index is committed before we clear anything;
	// clearCacheKeepLock runs after and skips the just-committed p.DBPath.
	_ = st.Close()
	if err := os.Rename(newDBPath, p.DBPath); err != nil {
		return err
	}
	committed = true // prevent defer from removing the now-live DB
	if err := clearCacheKeepLock(p); err != nil {
		return err
	}

	// Terminal marker: the index is now fully written and swapped. This is the
	// only "done" — the chunk phase logs "chunks done" and the graph pass runs
	// in between, so an earlier terminal log would read as a hang (#75).
	cliLogger().Info("index: done", logx.Phase("done"),
		"chunks", stats.Chunks, "files", stats.Files)

	if emptyErr := emptyIncludeErr(p.Root, stats.Chunks, ig); emptyErr != nil {
		return emptyErr
	}
	fmt.Fprintf(os.Stderr, "✓ reindexed %s\n", p.Root)
	fmt.Fprintf(os.Stderr, "  chunks: %d  files: %d  dim: %d\n", stats.Chunks, stats.Files, stats.Dim)
	if gstats != nil {
		_ = reportGraphStats(p.Root, gstats, "text")
	}
	return nil
}

// ─── watch ─────────────────────────────────────────────────────────────────

// envBool parses an env var as a boolean. Truthy: 1, on, true, yes
// (case-insensitive). Falsy: 0, off, false, no. Anything else (or
// unset) returns def.
func envBool(name string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "on", "true", "yes":
		return true
	case "0", "off", "false", "no":
		return false
	default:
		return def
	}
}

// envDuration reads a duration env var. Falls back to def with a
// warning on a parse error; honours def when unset.
func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a duration; using %s\n", name, raw, def)
		return def
	}
	return d
}
