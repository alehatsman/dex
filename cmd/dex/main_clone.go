package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/alehatsman/dex/internal/proj"
)

func cmdClone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	setHelp(fs,
		"Seed dst's index from src's (e.g. for a new git worktree). Follow with `dex index <dst>` to reconcile.",
		"dex clone [flags] <src-path> <dst-path>")
	force := fs.Bool("force", false, "overwrite dst's index if it already exists")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("clone needs <src-path> <dst-path>")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	src, err := proj.Resolve(rest[0], base)
	if err != nil {
		return fmt.Errorf("resolve src: %w", err)
	}
	dst, err := proj.Resolve(rest[1], base)
	if err != nil {
		return fmt.Errorf("resolve dst: %w", err)
	}
	if src.ID == dst.ID {
		return fmt.Errorf("src and dst resolve to the same project root (%s); nothing to clone", src.Root)
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("src has no index at %s — run `dex index %s` first", src.DBPath, src.Root)
		}
		return err
	}
	if _, err := os.Stat(dst.DBPath); err == nil {
		if !*force {
			return fmt.Errorf("dst already has an index at %s — pass --force to overwrite or `dex nuke %s` first", dst.DBPath, dst.Root)
		}
		if err := os.RemoveAll(dst.CacheDir); err != nil {
			return fmt.Errorf("remove existing dst cache: %w", err)
		}
	}
	if err := dst.EnsureCacheDir(); err != nil {
		return err
	}
	// Copy index.db as an independent file. SQLite WAL files are not copied —
	// they're either already checkpointed (idle index) or will be rebuilt on
	// next open.
	if err := copyFile(src.DBPath, dst.DBPath); err != nil {
		return fmt.Errorf("copy index: %w", err)
	}
	// Re-tag project_root so `reindex --all` / status see this cache
	// as belonging to dst, not src. A subsequent `dex index <dst>`
	// would also do this, but tagging now keeps the cache discoverable
	// even before the first reconcile.
	if err := retagProjectRoot(ctx, dst.DBPath, dst.Root); err != nil {
		return fmt.Errorf("retag project root: %w", err)
	}
	fmt.Printf("✓ cloned %s → %s\n", src.Root, dst.Root)
	fmt.Printf("  next: `dex index %s` will reconcile any files that differ between the two trees (incremental — only changed chunks are re-embedded).\n", dst.Root)
	return nil
}

// retagProjectRoot opens the cloned DB just long enough to overwrite
// the project_root meta key, so the dst cache no longer claims to be
// src's index.
func retagProjectRoot(ctx context.Context, dbPath, root string) error {
	st, err := openStore(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.SetProjectRoot(ctx, root)
}

// copyFile copies src to dst as a distinct file. It deliberately does NOT
// hard-link: clone produces an independently mutable index (dst is retagged
// and later reconciled), so a shared inode would let a write to one corrupt
// the other — retagging dst's project_root would overwrite src's (#517).
func copyFile(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ─── mcp ───────────────────────────────────────────────────────────────────
