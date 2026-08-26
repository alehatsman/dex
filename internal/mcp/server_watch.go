// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graphrefresh"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/veccache"
	"github.com/alehatsman/dex/internal/watch"
)

// cwdWarned dedups the cwd-fallback warning to one line per distinct root per
// process, so a chatty tool loop can't spam the log.
var cwdWarned sync.Map

// warnCwdFallback emits a single stderr warning when resolveProject had no
// explicit project_root and no client root to fall back on, and defaulted to the
// server's cwd. This is the safety net that keeps a wrong-worktree read from
// being silent (#120).
func warnCwdFallback(wd string) {
	if _, loaded := cwdWarned.LoadOrStore(wd, struct{}{}); loaded {
		return
	}
	slog.Warn("resolved project_root from server cwd; pass project_root or start the client inside the worktree to target it", "cwd", wd)
}

// markForeground records that a foreground query just touched project p
// so the background summary drainer (here or in another process) yields
// to interactive work for the configured YieldWindow. Best-effort: a
// touch failure must never affect the query. One cheap syscall per
// query (queries are agent-paced, so no throttle is needed).
func (s *Server) markForeground(p *proj.Project) {
	if p != nil {
		_ = p.MarkActivity()
	}
}

// watcherCooldown is how long runWatcher waits before allowing a respawn
// after a persistent setup or watch error.
const watcherCooldown = 5 * time.Minute

// ensureWatcher lazily spawns a Watcher goroutine for this project.
// Concurrency-safe; respawns are blocked during a cooldown period so a
// persistent error (bad inotify, missing index) doesn't leak one goroutine
// per MCP request (#716).
func (s *Server) ensureWatcher(p *proj.Project) {
	if s == nil || s.runCtx == nil || s.runCtx.Err() != nil {
		return
	}
	if !s.AutoWatch.Enabled {
		return
	}
	for {
		actual, loaded := s.watchers.LoadOrStore(p.ID, struct{}{})
		if !loaded {
			// We stored the running marker — spawn the watcher.
			s.watcherWG.Add(1)
			go s.runWatcher(p)
			return
		}
		switch v := actual.(type) {
		case struct{}:
			return // watcher already running
		case time.Time:
			if time.Now().Before(v) {
				return // still in cooldown
			}
			// Cooldown expired: atomically replace with running marker.
			if s.watchers.CompareAndSwap(p.ID, actual, struct{}{}) {
				s.watcherWG.Add(1)
				go s.runWatcher(p)
			}
			return
		default:
			return
		}
	}
}

// runWatcher owns the lifecycle of a single project's Watcher inside
// the MCP server. Closes its store + ignores when the goroutine
// returns so RunStdio's defer s.watcherWG.Wait() drains cleanly.
func (s *Server) runWatcher(p *proj.Project) {
	defer s.watcherWG.Done()

	// setCooldown replaces the running marker with a cooldown timestamp so
	// ensureWatcher won't immediately respawn on the next request.
	setCooldown := func() { s.watchers.Store(p.ID, time.Now().Add(watcherCooldown)) }

	logger := s.AutoWatch.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	if err := proj.CheckIndexable(p, false); err != nil {
		logger.Info("mcp watch: skipping (not indexable)", "root", p.Root, "err", err)
		setCooldown()
		return
	}
	if err := p.EnsureCacheDir(); err != nil {
		logger.Warn("mcp watch: cache dir failed", "root", p.Root, "err", err)
		setCooldown()
		return
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		logger.Warn("mcp watch: store open failed", "root", p.Root, "err", err)
		setCooldown()
		return
	}
	ig, err := ignore.New(p.Root)
	if err != nil {
		logger.Warn("mcp watch: ignore init failed", "root", p.Root, "err", err)
		setCooldown()
		return
	}

	// Wrap the embed client with a content-addressed vector cache so watch
	// re-indexes reuse vectors for unchanged content instead of re-embedding
	// (#121). The cache lives in p.VecCacheDir(), shared across a repo's
	// worktrees (#123). Best-effort: on open failure fall back to the raw
	// client. Only the indexing passes use it — the query path keeps the
	// unwrapped s.EmbedClient.
	indexEm := s.EmbedClient
	if s.EmbedClient != nil {
		if vc, err := veccache.Open(filepath.Join(p.VecCacheDir(), veccache.FileName), veccache.MaxRowsFromEnv()); err == nil {
			indexEm = embed.WithCache(s.EmbedClient, vc)
			defer func() { _ = vc.Close() }()
		} else {
			logger.Warn("mcp watch: vec cache open failed", "root", p.Root, "err", err)
		}
	}

	ixOpts := index.Options{
		Logger:      logger,
		Concurrency: s.AutoWatch.IndexConcurrency,
	}
	ix := index.New(p, st, indexEm, ig, ixOpts)

	wOpts := watch.Options{
		Debounce: s.AutoWatch.Debounce,
		MaxDelay: s.AutoWatch.MaxDelay,
		Logger:   logger,
		// Refresh the graph lane after each chunk reindex so call-graph
		// queries (callers/callees/impact/path) stay as fresh as semantic
		// search. Without this the MCP watcher updated chunks but left the
		// graph stale until the next full `dex index` (#327). Mirrors the
		// CLI `dex watch` afterIndex hook.
		AfterIndex: func(c context.Context) error {
			if _, err := graphrefresh.RunPhase(c, p, st, false, logger); err != nil {
				return err
			}
			if indexEm != nil {
				if _, err := graphrefresh.EmbedNodes(c, st, indexEm, false, logger); err != nil {
					logger.Warn("mcp watch: graph-embed failed", "root", p.Root, "err", err)
				}
			}
			return nil
		},
	}
	w := watch.New(ix, ig, p.Root, wOpts)
	logger.Info("mcp watch: starting", "root", p.Root)
	if err := w.Run(s.runCtx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Warn("mcp watch: exited with error", "root", p.Root, "err", err)
		setCooldown()
		return
	}
	// Clean exit (context canceled = server shutdown): free the slot entirely
	// so a server restart can spawn fresh.
	s.watchers.Delete(p.ID)
}
