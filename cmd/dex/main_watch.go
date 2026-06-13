package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/watch"
)

func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	setHelp(fs,
		"Keep the index fresh as files change (foreground; runs chunk + graph after each debounce).",
		"dex watch [flags] <path>")
	verbose := fs.Bool("v", false, "verbose")
	force := fs.Bool("force", false, "bypass protected-path and git-tree guards")
	debounce := fs.Duration("debounce", 500*time.Millisecond, "quiet window before re-indexing")
	waitLock := fs.Bool("wait", false, "if another dex indexer is running on this project, wait for it to finish instead of skipping")
	breakLock := fs.Bool("break-lock", false, "discard an existing project lockfile (use only when the prior holder is gone)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("watch needs exactly one path argument")
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
	if err := p.EnsureCacheDir(); err != nil {
		return err
	}
	lk, err := acquireProjectLock(ctx, p, "watch", "chunk", *waitLock, *breakLock)
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
	ig, err := ignore.New(p.Root)
	if err != nil {
		return err
	}
	warnIfNoInclude(ig, p.Root)
	logger := cliLogger()

	ixOpts := index.Options{
		Verbose:     *verbose,
		Logger:      logger,
		Concurrency: envInt("DEX_INDEX_CONCURRENCY", 0),
	}
	ix := index.New(p, st, newEmbedClient(st.EmbedModel()), ig, ixOpts)

	// Refresh the Go static graph after each chunk-index flush. The
	// graph layer lives in the same SQLite file, so the chunk run has
	// already released the writer when this fires.
	afterIndex := func(c context.Context) error {
		if _, err := runGraphPhase(c, p, st, *verbose); err != nil {
			return err
		}
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			if _, err := embedGraphNodes(c, st, em, false); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ graph-embed failed: %v\n", err)
			}
		}
		return nil
	}
	wOpts := watch.Options{
		Debounce:   *debounce,
		Verbose:    *verbose,
		Logger:     logger,
		AfterIndex: afterIndex,
	}
	w := watch.New(ix, ig, p.Root, wOpts)
	return w.Run(ctx)
}

// ─── clone ─────────────────────────────────────────────────────────────────

// cmdClone seeds dst's per-project cache from src's. Useful when the same
// repository is checked out in multiple locations (e.g. git worktrees,
// branch-per-folder workflows). Chunks are keyed by (relative path,
// content sha1), so the copied index is correct for any file that exists
// at the same path with the same content in dst; differing files get
// reconciled on the next `dex index <dst>` (incremental — only
// changed chunks are re-embedded).
