package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/lock"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/veccache"
)

func acquireProjectLock(ctx context.Context, p *proj.Project, cmdName, phase string, wait, breakLock bool) (*lock.Lock, error) {
	host, _ := os.Hostname()
	h := lock.Holder{
		PID:     os.Getpid(),
		Host:    host,
		Command: cmdName,
		Phase:   phase,
		Started: time.Now(),
	}
	if breakLock {
		l, err := lock.Steal(p.LockPath, h)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, lock.ErrLocked) {
			return nil, err
		}
		// A live holder is not broken — that would flock a fresh inode and leave
		// two indexers writing the same store (#667). Say so and exit cleanly.
		holder, _ := lock.ReadHolder(p.LockPath)
		fmt.Fprintf(os.Stderr, "another dex indexer is still running on %s%s\n", p.Root, describeHolder(holder))
		fmt.Fprintln(os.Stderr, "  --break-lock clears only a stale lock; a live holder is not broken — stop it first")
		return nil, nil
	}
	if wait {
		return lock.AcquireWait(ctx, p.LockPath, h)
	}
	l, err := lock.Acquire(p.LockPath, h)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, lock.ErrLocked) {
		return nil, err
	}
	holder, _ := lock.ReadHolder(p.LockPath)
	fmt.Fprintf(os.Stderr, "another dex indexer is running on %s%s\n", p.Root, describeHolder(holder))
	fmt.Fprintln(os.Stderr, "  pass --wait to block, or --break-lock if the holder is gone")
	return nil, nil
}

// describeHolder formats a parenthetical for the contention message.
// Returns "" when no holder info is available.
func describeHolder(h *lock.Holder) string {
	if h == nil {
		return ""
	}
	var parts []string
	if h.PID != 0 {
		parts = append(parts, fmt.Sprintf("pid %d", h.PID))
	}
	if h.Command != "" {
		parts = append(parts, fmt.Sprintf("cmd=%s", h.Command))
	}
	if h.Phase != "" {
		parts = append(parts, fmt.Sprintf("phase=%s", h.Phase))
	}
	if !h.Started.IsZero() {
		parts = append(parts, fmt.Sprintf("for %s", time.Since(h.Started).Round(time.Second)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// clearCacheKeepLock removes everything inside p.CacheDir except the
// lock file and the live index (p.DBPath). Used by `reindex` after the
// new index has been renamed into place, to sweep old ephemeral files
// (WAL, SHM, chunk vectors, etc.) without touching the committed DB or
// the lock.
func clearCacheKeepLock(p *proj.Project) error {
	entries, err := os.ReadDir(p.CacheDir)
	if err != nil {
		return err
	}
	lockBase := filepath.Base(p.LockPath)
	dbBase := filepath.Base(p.DBPath)
	for _, e := range entries {
		if e.Name() == lockBase || e.Name() == dbBase {
			continue
		}
		// Preserve the content-addressed vector cache (and its WAL/SHM) so a
		// reindex reuses vectors for unchanged content instead of re-embedding
		// (#121). It survives precisely because this sweep skips it — and it is
		// held open across this call, so removing it would corrupt a live conn.
		if strings.HasPrefix(e.Name(), veccache.FileName) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(p.CacheDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cmdIndexDispatch peels off the `status` subcommand before
// falling through to `cmdIndex` (which expects a single path arg).
