package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
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
		roots, err := knownProjectRoots(ctx, base)
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

// restoreNotes re-adds rescued knowledge facts into a freshly-rebuilt store
// (#647). KnowledgeRestore (not KnowledgeAdd) preserves each fact's scope
// binding and salience signal (created_at / counters) across the rebuild —
// otherwise the first reindex silently unscopes every scoped note and resets
// its ranking (#678). Best-effort and idempotent (dedups by body); a per-fact
// failure is skipped, never fatal to the reindex. Embeddings backfill lazily on
// the next semantic recall.
func restoreNotes(ctx context.Context, st *store.Store, facts []store.KnowledgeBackup) {
	if len(facts) == 0 {
		return
	}
	restored := 0
	for _, f := range facts {
		if err := st.KnowledgeRestore(ctx, f); err == nil {
			restored++
		}
	}
	if restored > 0 {
		fmt.Fprintf(os.Stderr, "  notes: preserved %d across reindex\n", restored)
	}
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
	// Rescue the knowledge store (#647/#648) — the clear below drops the whole DB
	// and the user's notes live in it. The read is migrate-FREE (raw sqlite), so
	// notes survive even a reindex triggered by a schema mismatch, when the
	// migrate-gated open above fails. Best-effort.
	savedNotes, _ := store.ExportKnowledgeRaw(ctx, p.DBPath)
	if err := clearCacheKeepLock(p); err != nil {
		return err
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	// Restore rescued notes into the fresh store. KnowledgeAdd dedups by body so
	// this is idempotent; embeddings backfill lazily on the next semantic recall.
	restoreNotes(ctx, st, savedNotes)
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	warnIfNoInclude(ig, p.Root)
	ixOpts := index.Options{Verbose: verbose, Logger: cliLogger(), Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0)}
	ix := index.New(p, st, newEmbedClient(priorEmbedModel), ig, ixOpts)
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
	fmt.Fprintf(os.Stderr, "✓ reindexed %s\n", p.Root)
	fmt.Fprintf(os.Stderr, "  chunks: %d  files: %d  dim: %d\n", stats.Chunks, stats.Files, stats.Dim)
	if gstats != nil {
		_ = reportGraphStats(p.Root, gstats, "text")
	}
	if gstats != nil {
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			if _, err := embedGraphNodes(ctx, st, em, false); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed failed for %s: %v\n", p.Root, err)
			}
		}
	}
	return nil
}

// knownProjectRoots walks the index dir, opening each per-project index
// and reading the recorded `project_root` meta. Entries written before
// that meta existed are reported to stderr and skipped — the user can
// `dex nuke <path>` + `dex index <path>` once to re-record it.
func knownProjectRoots(ctx context.Context, base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(base, e.Name(), "index.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		st, err := openStore(ctx, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: open: %v\n", e.Name(), err)
			continue
		}
		root, err := st.ProjectRoot(ctx)
		st.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", e.Name(), err)
			continue
		}
		if root == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: no recorded project_root (pre-migration index)\n", e.Name())
			continue
		}
		roots = append(roots, root)
	}
	return roots, nil
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
