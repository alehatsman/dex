package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/lock"
	"github.com/alehatsman/dex/internal/logx"
	"github.com/alehatsman/dex/internal/store"
	"golang.org/x/sync/errgroup"
)

// drainHolder describes this process as the holder of a project's
// summary-drain lock (DrainLockPath). Distinct Command from the index
// lock so `dex lock`-style introspection can tell the two apart.
func (ix *Indexer) drainHolder(phase string) lock.Holder {
	host, _ := os.Hostname()
	return lock.Holder{
		PID:     os.Getpid(),
		Host:    host,
		Command: "summary-drain",
		Phase:   phase,
		Started: time.Now(),
	}
}

// foregroundBusy reports whether a foreground query ran within
// Options.YieldWindow. Used by the idle drainer to defer background
// summary generation while an agent is actively querying. Always false
// when YieldWindow <= 0 (feature off).
func (ix *Indexer) foregroundBusy() bool {
	if ix.Options.YieldWindow <= 0 || ix.Proj == nil {
		return false
	}
	ts, ok := ix.Proj.LastActivity()
	return ok && time.Since(ts) < ix.Options.YieldWindow
}

// durabilityWindow caps how many pending rows are loaded into memory per
// DrainPendingSummariesBatch call when the caller passes max=0 (unbounded).
// Progress is committed to the store after every window; a crash loses at
// most this many generated summaries. Distinct from pacing: no sleep is
// injected — throughput is unchanged.
const durabilityWindow = 64

// batchForPace caps rows per whole-queue drain call. 0 (unbounded —
// use durabilityWindow) when pacing is off; a bounded batch when
// SummaryPace > 0 so DrainPendingSummaries can sleep between them.
func (ix *Indexer) batchForPace() int {
	if ix.Options.SummaryPace > 0 {
		return 10
	}
	return 0
}

// drainItem processes one pending summary row. Returns the result to upsert
// (nil on error or cache hit) and whether the row is stale. Errors are
// logged and the attempt counter bumped; they are never propagated so the
// errgroup can continue to the next row. bumpCtx is the outer context used
// for BumpPendingAttempts so it survives errgroup cancellation.
func (ix *Indexer) drainItem(ctx, bumpCtx context.Context, p store.PendingSummary) (*drainResult, bool) {
	switch p.Kind {
	case chunk.KindFileSummary:
		res, stale, err := ix.processFileSummary(ctx, p)
		if err != nil {
			ix.drainLog.Warn("file summary drain failed", "id", p.ID, "path", p.Path, "err", err)
			_ = ix.Store.BumpPendingAttempts(bumpCtx, p.ID, err.Error())
			return nil, false
		}
		return res, stale
	case chunk.KindChunkSummary:
		res, stale, err := ix.processChunkSummary(ctx, p)
		if err != nil {
			ix.drainLog.Warn("chunk summary drain failed", "id", p.ID, "path", p.Path, "start_line", p.StartLine, "err", err)
			_ = ix.Store.BumpPendingAttempts(bumpCtx, p.ID, err.Error())
			return nil, false
		}
		return res, stale
	default:
		ix.drainLog.Warn("unknown pending kind", "id", p.ID, "kind", p.Kind)
		_ = ix.Store.BumpPendingAttempts(bumpCtx, p.ID, "unknown kind")
		return nil, false
	}
}

// DrainPendingSummariesBatch processes up to `max` rows from
// pending_summaries (file_summary + chunk_summary kinds). Pass 0 for
// "no limit" — drain everything currently queued.
//
// Returns (generated, remaining, dirtiedDirs, err):
//   - generated: summaries upserted this call (excludes stale-drops
//     and cache hits).
//   - remaining: queue depth observed AFTER this batch. Caller can
//     loop while remaining > 0 to bound per-call latency.
//   - dirtiedDirs: directories whose file_summary chunks were committed
//     in this batch. Pass to CascadePackageRepoSummaries for an
//     incremental cascade (only re-summarizes affected packages).
//
// Does NOT cascade. Callers that want package_summary / repo_summary
// refreshed must invoke CascadePackageRepoSummaries separately —
// typically once the queue reaches remaining == 0.
//
// Cancellation is cooperative: per-row chat calls honour ctx, and
// rows already upserted + deleted from the queue stay committed even
// if ctx ends mid-batch. This makes the batch a safe unit of work for
// a watcher's idle hook to schedule and preempt.
func (ix *Indexer) DrainPendingSummariesBatch(ctx context.Context, max int) (generated, remaining int, dirtiedDirs []string, err error) {
	if ix.Options.Chat == nil {
		return 0, 0, nil, fmt.Errorf("DrainPendingSummariesBatch: chat client not configured")
	}
	// Same embed-model gate as Run: the drainer also embeds and upserts.
	if err := ix.Store.EnsureEmbedModel(ctx, ix.Embed.ModelName()); err != nil {
		return 0, 0, nil, err
	}
	startTime := time.Now()

	// Apply the durability window: when max=0 (unbounded), cap the load so
	// each call commits at most durabilityWindow rows. The outer loop in
	// DrainPendingSummaries iterates until remaining==0, so throughput is
	// unaffected — only the in-memory window shrinks.
	limit := max
	if limit == 0 {
		limit = durabilityWindow
	}
	pending, err := ix.Store.ListPendingSummaries(ctx, limit)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("list pending: %w", err)
	}
	if len(pending) == 0 {
		return 0, 0, nil, nil
	}
	ix.drainLog.Info("drain: batch starting", "pending", len(pending), "limit", limit)

	conc := ix.Options.SummaryConcurrency
	if conc < 1 {
		conc = 1
	}
	results := make([]*drainResult, len(pending))
	stale := make([]int64, 0)
	var staleMu sync.Mutex
	addStale := func(id int64) {
		staleMu.Lock()
		stale = append(stale, id)
		staleMu.Unlock()
	}

	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(conc)
	for i := range pending {
		p := pending[i]
		eg.Go(func() error {
			res, isStale := ix.drainItem(egctx, ctx, p)
			if isStale {
				addStale(p.ID)
			} else if res != nil {
				results[i] = res
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return 0, 0, nil, err
	}

	// Compact successful results, then embed + upsert in batches.
	successful := make([]*drainResult, 0, len(results))
	for _, r := range results {
		if r != nil {
			successful = append(successful, r)
		}
	}

	// Track which directories had file_summary chunks committed so
	// CascadePackageRepoSummaries can do an incremental (dirty-dir) scan.
	dirtiedDirSet := make(map[string]struct{})

	if len(successful) > 0 {
		batchSize := ix.Embed.BatchSize()
		if batchSize <= 0 {
			batchSize = 32
		}
		totalBatches := (len(successful) + batchSize - 1) / batchSize
		ix.drainLog.Info("drain: embedding summaries",
			"chunks", len(successful),
			"batches", totalBatches,
			"batch_size", batchSize)
		for start := 0; start < len(successful); start += batchSize {
			// Honour cancellation between batches: the watcher's idle hook
			// can interrupt long drains; without this check we'd still
			// embed + upsert one full batch wave after ctx.Done.
			if err := ctx.Err(); err != nil {
				return generated, 0, nil, err
			}
			end := start + batchSize
			if end > len(successful) {
				end = len(successful)
			}
			batch := successful[start:end]
			texts := make([]string, len(batch))
			for i, r := range batch {
				texts[i] = r.chunk.EmbedText()
			}
			batchStart := time.Now()
			vecs, embErr := ix.Embed.Embed(ctx, texts)
			if embErr != nil {
				return generated, 0, nil, fmt.Errorf("embed: %w", embErr)
			}
			rows := make([]store.PendingChunk, len(batch))
			for i, r := range batch {
				rows[i] = store.PendingChunk{
					Path:       r.chunk.Path,
					Kind:       r.chunk.Kind,
					Name:       r.chunk.Name,
					StartLine:  r.chunk.StartLine,
					EndLine:    r.chunk.EndLine,
					ContentSHA: r.sha,
					Content:    r.chunk.Content,
					Vec:        vecs[i],
				}
			}
			if upErr := ix.Store.UpsertMany(ctx, rows, startTime); upErr != nil {
				return generated, 0, nil, fmt.Errorf("upsert: %w", upErr)
			}
			// Only delete pending rows after the upsert succeeds.
			for _, r := range batch {
				if delErr := ix.Store.DeletePendingSummary(ctx, r.pendingID); delErr != nil {
					return generated, 0, nil, fmt.Errorf("delete pending: %w", delErr)
				}
				// Collect dirtied dirs from committed file_summary chunks.
				if r.chunk.Kind == chunk.KindFileSummary {
					dirtiedDirSet[filepath.Dir(r.chunk.Path)] = struct{}{}
				}
			}
			generated += len(batch)
			ix.drainLog.Info("drain: embed batch",
				"batch", start/batchSize+1,
				"batch_total", totalBatches,
				"chunks", len(batch),
				logx.DurMS(time.Since(batchStart)))
		}
	}

	// Drop stale rows (source content changed since enqueue). The next
	// index --summarize-defer run will re-enqueue with the new SHA.
	for _, id := range stale {
		if err := ix.Store.DeletePendingSummary(ctx, id); err != nil {
			return generated, 0, nil, fmt.Errorf("delete stale pending: %w", err)
		}
	}
	if len(stale) > 0 {
		ix.drainLog.Info("drain: dropped stale rows", "count", len(stale))
	}

	remaining, err = ix.Store.CountPendingSummaries(ctx)
	if err != nil {
		// Surface the error rather than reporting remaining=0: a false
		// "queue empty" would make DrainPendingSummaries break its loop
		// and IdleSummaryDrainer run the cascade as if the queue drained.
		return generated, 0, nil, fmt.Errorf("count pending after drain: %w", err)
	}
	if generated > 0 {
		_ = ix.Store.SetLastSummarizedAt(ctx, time.Now())
	}

	// Deduplicate dirtiedDirs from set.
	if len(dirtiedDirSet) > 0 {
		dirtiedDirs = make([]string, 0, len(dirtiedDirSet))
		for d := range dirtiedDirSet {
			dirtiedDirs = append(dirtiedDirs, d)
		}
	}

	ix.drainLog.Info("drain: batch done",
		"generated", generated,
		"stale_dropped", len(stale),
		"remaining", remaining,
		logx.DurMS(time.Since(startTime)))
	return generated, remaining, dirtiedDirs, nil
}

// CascadePackageRepoSummaries regenerates any missing package_summary
// and repo_summary chunks from the current file_summary state of the
// chunks table. No-op when no file_summary chunks exist yet.
//
// dirtyDirs is an optional hint from DrainPendingSummariesBatch: when
// non-nil, only the packages in those directories are re-evaluated
// (incremental cascade). Pass nil for a full scan — used by
// DrainPendingSummaries and external callers that don't have a hint.
//
// Exposed so external callers (e.g. the watcher's idle hook) can run
// the cascade independently of the per-batch drainer — typically once
// DrainPendingSummariesBatch reports remaining == 0.
func (ix *Indexer) CascadePackageRepoSummaries(ctx context.Context, dirtyDirs []string) (int, error) {
	if ix.Options.Chat == nil {
		return 0, fmt.Errorf("CascadePackageRepoSummaries: chat client not configured")
	}
	if err := ix.Store.EnsureEmbedModel(ctx, ix.Embed.ModelName()); err != nil {
		return 0, err
	}
	gen, err := ix.cascadePackageAndRepo(ctx, time.Now(), dirtyDirs)
	if err == nil && gen > 0 {
		_ = ix.Store.SetLastSummarizedAt(ctx, time.Now())
	}
	return gen, err
}

// IdleSummaryDrainer returns a callback suitable for
// watch.Options.OnIdle: it drains pending_summaries in batches of
// batchSize and cascades package + repo summaries once the queue is
// empty. Returns nil when the Indexer has no chat client configured
// (caller must fall through with OnIdle=nil).
//
// Stop conditions encoded in the callback:
//   - queue empty → cascade then signal done=true.
//   - batch made no progress (chat endpoint dead → all rows fail) →
//     done=true so we don't busy-loop; the next flush re-arms.
//   - underlying batch errors → (true, err); the watcher logs and
//     stops the cycle.
//
// Shared by `dex watch` and the MCP auto-watcher.
func (ix *Indexer) IdleSummaryDrainer(batchSize int) func(context.Context) (bool, error) {
	if ix.Options.Chat == nil {
		return nil
	}
	if batchSize <= 0 {
		batchSize = 10
	}
	logger := ix.drainLog
	verbose := ix.Options.Verbose
	// Exponential backoff state shared across calls. The watcher may
	// invoke the returned closure many times within a session; without
	// this, a chat endpoint that flaps once would still print a warning
	// every idle tick and chew through cycles re-checking the queue.
	const (
		maxConsecutiveNoProgress = 3
		initialBackoff           = 30 * time.Second
		maxBackoff               = 30 * time.Minute
	)
	var (
		consecutiveNoProgress int
		nextAttempt           time.Time
		currentBackoff        time.Duration
	)
	return func(ctx context.Context) (bool, error) {
		if !nextAttempt.IsZero() && time.Now().Before(nextAttempt) {
			// Still inside the backoff window — skip without logging
			// so we don't spam the slog. The watcher will retry on the
			// next idle tick.
			return true, nil
		}
		// Foreground-yield: an agent queried recently, so leave the GPU
		// to interactive work. Re-arm (done=false) to retry once quiet
		// rather than waiting for the next fs flush.
		if ix.foregroundBusy() {
			return false, nil
		}
		// Cross-process dedupe: only one process drains a project's
		// queue at a time. If another holds the lock, skip and re-arm.
		dl, lerr := lock.Acquire(ix.Proj.DrainLockPath, ix.drainHolder("idle-drain"))
		if errors.Is(lerr, lock.ErrLocked) {
			return false, nil
		}
		if lerr != nil {
			return true, lerr
		}
		defer func() { _ = dl.Release() }()

		before, err := ix.Store.CountPendingSummaries(ctx)
		if err != nil {
			// No baseline to compare against — skip this tick and re-arm
			// rather than mistaking the missing count for no-progress and
			// tripping the backoff. A persistent DB error is surfaced by
			// the DrainPendingSummariesBatch call below on the next tick.
			if verbose {
				logger.Warn("idle drain: count pending failed, retrying", "err", err)
			}
			return false, nil
		}
		gen, after, dirs, err := ix.DrainPendingSummariesBatch(ctx, batchSize)
		if err != nil {
			return true, err
		}
		if after == 0 {
			consecutiveNoProgress = 0
			nextAttempt = time.Time{}
			currentBackoff = 0
			cascadeGen, err := ix.CascadePackageRepoSummaries(ctx, dirs)
			if err != nil {
				return true, err
			}
			if verbose && (gen > 0 || cascadeGen > 0) {
				logger.Info("idle drain: complete", "summaries", gen, "cascade", cascadeGen)
			}
			return true, nil
		}
		if after >= before {
			consecutiveNoProgress++
			if consecutiveNoProgress >= maxConsecutiveNoProgress {
				if currentBackoff == 0 {
					currentBackoff = initialBackoff
				} else {
					currentBackoff *= 2
					if currentBackoff > maxBackoff {
						currentBackoff = maxBackoff
					}
				}
				nextAttempt = time.Now().Add(currentBackoff)
				logger.Warn("idle drain: no progress, backing off",
					"remaining", after, "consecutive_failures", consecutiveNoProgress,
					"backoff", currentBackoff)
			} else if verbose {
				logger.Warn("idle drain: no progress",
					"remaining", after, "consecutive_failures", consecutiveNoProgress)
			}
			return true, nil
		}
		// Progress on this batch — clear the failure counter.
		consecutiveNoProgress = 0
		nextAttempt = time.Time{}
		currentBackoff = 0
		if verbose {
			logger.Info("idle drain: batch", "generated", gen, "remaining", after)
		}
		return false, nil
	}
}

// DrainPendingSummaries drains the entire queue then cascades. This is
// the all-in-one entry point used by `dex index summarize`; callers
// that need to yield between rows (a watcher's idle hook, for
// example) should compose DrainPendingSummariesBatch with
// CascadePackageRepoSummaries instead.
func (ix *Indexer) DrainPendingSummaries(ctx context.Context) (int, error) {
	if ix.Options.Chat == nil {
		return 0, fmt.Errorf("DrainPendingSummaries: chat client not configured")
	}
	// Coordinate with any background drainer via the per-project lock so
	// a manual `dex index summarize` and a watcher don't double-generate
	// the same rows. AcquireWait yields once the background drainer
	// releases (it does so after every batch). Best-effort: if the lock
	// layer errors, fall through and drain anyway rather than refuse.
	if dl, err := lock.AcquireWait(ctx, ix.Proj.DrainLockPath, ix.drainHolder("manual-drain")); err == nil {
		defer func() { _ = dl.Release() }()
	}
	total := 0
	for {
		gen, remaining, _, err := ix.DrainPendingSummariesBatch(ctx, ix.batchForPace())
		if err != nil {
			return total, err
		}
		total += gen
		if remaining == 0 {
			break
		}
		// When paced, DrainPendingSummariesBatch returns after one bounded
		// batch (not the whole queue), so sleep between batches to leave
		// GPU headroom for foreground work before draining the rest.
		if ix.Options.SummaryPace > 0 {
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(ix.Options.SummaryPace):
			}
		}
	}
	ix.drainLog.Info("drain: cascading package + repo summaries")
	cascadeGen, err := ix.CascadePackageRepoSummaries(ctx, nil) // nil = full scan
	if err != nil {
		return total, err
	}
	return total + cascadeGen, nil
}

// processFileSummary handles one pending file_summary row. Returns
// (result, stale, err). `stale=true` means the file's current content
// no longer matches what was queued — the drainer should drop the row.
func (ix *Indexer) processFileSummary(ctx context.Context, p store.PendingSummary) (*drainResult, bool, error) {
	fullPath := filepath.Join(ix.Proj.Root, p.Path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil // file was removed; drop pending
		}
		return nil, false, fmt.Errorf("read %s: %w", fullPath, err)
	}
	slice := data
	if len(slice) > summarizeCap {
		slice = slice[:summarizeCap]
	}
	currentSHA := chunkSHA(fileSummaryPromptVersion + "\x00" + string(slice))
	if currentSHA != p.ContentSHA {
		return nil, true, nil // file changed; drop pending
	}
	subjects := RecentCommitSubjects(ctx, ix.Proj.Root, p.Path, 3)
	summary, err := summarizeFile(ctx, ix.Options.Chat, ix.Options.SummaryModels.File, p.Path, slice, subjects)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(summary) == "" {
		return nil, true, nil // empty response; drop rather than retry
	}
	endLine := p.EndLine
	if endLine <= 0 {
		endLine = chunk.LineCount(data)
	}
	return &drainResult{
		pendingID: p.ID,
		chunk: chunk.Chunk{
			Path:      p.Path,
			Kind:      chunk.KindFileSummary,
			StartLine: 1,
			EndLine:   endLine,
			Content:   summary,
		},
		sha: p.ContentSHA,
	}, false, nil
}

// processChunkSummary handles one pending chunk_summary row. The
// source chunk's content is looked up by (path, source_sha1) — if it's
// no longer in the chunks table, the source has been pruned (file
// changed or removed), so we drop the row as stale.
func (ix *Indexer) processChunkSummary(ctx context.Context, p store.PendingSummary) (*drainResult, bool, error) {
	content, err := ix.Store.ChunkContent(ctx, p.Path, p.SourceSHA)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, true, nil // source chunk gone; drop pending
		}
		return nil, false, err
	}
	// Recompute the expected sumSHA from the recovered content; if it
	// doesn't match what was queued, something is inconsistent and we
	// drop the row rather than upsert under the wrong identity.
	expectedSumSHA := chunkSHA(chunk.KindChunkSummary + ":" + content)
	if expectedSumSHA != p.ContentSHA {
		return nil, true, nil
	}
	sourceChunk := chunk.Chunk{
		Path:      p.Path,
		Kind:      p.ChunkKind,
		Name:      p.ChunkName,
		StartLine: p.StartLine,
		EndLine:   p.EndLine,
		Content:   content,
	}
	summary, err := summarizeChunk(ctx, ix.Options.Chat, ix.Options.SummaryModels.Chunk, p.Path, sourceChunk)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(summary) == "" {
		return nil, true, nil
	}
	return &drainResult{
		pendingID: p.ID,
		chunk: chunk.Chunk{
			Path:      p.Path,
			Kind:      chunk.KindChunkSummary,
			StartLine: p.StartLine,
			EndLine:   p.EndLine,
			Content:   summary,
		},
		sha: p.ContentSHA,
	}, false, nil
}

// drainResult mirrors the anonymous struct used by DrainPendingSummaries.
// Lifted to package scope so the processX helpers can return it.
type drainResult struct {
	pendingID int64
	chunk     chunk.Chunk
	sha       string
}

// pkgJob is one directory whose package_summary needs (re)generation.
// Hoisted from cascadePackageAndRepo so the planner and executor can
// pass them around as proper values.
type pkgJob struct {
	dir       string
	filePaths []string
	pkgSHA    string
}

// cascadePackageAndRepo regenerates any missing package_summary and
// repo_summary chunks based on the current file_summary / package_summary
// state of the chunks table. Mirrors Run()'s Pass 5 and Pass 6, but
// reads its inputs from the store instead of from in-flight pkgFiles.
//
// dirtyDirs is an optional filter: when non-nil, only those directories
// are considered for package-summary regeneration (incremental cascade).
// When nil, all directories are scanned (full cascade).
func (ix *Indexer) cascadePackageAndRepo(ctx context.Context, startTime time.Time, dirtyDirs []string) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	allSHAs, err := ix.Store.FileSummarySHAs(ctx)
	if err != nil {
		return 0, fmt.Errorf("file summary SHAs: %w", err)
	}
	if len(allSHAs) == 0 {
		return 0, nil
	}
	dirs, pkgFiles := groupSummariesByDir(allSHAs)

	// When a dirty-dirs hint is available, restrict pkgFiles to only
	// those directories. This turns an O(all_packages) cascade into an
	// O(changed_packages) cascade on every idle-drain tick.
	if dirtyDirs != nil {
		filtered := make(map[string][]pkgFileEntry, len(dirtyDirs))
		for _, d := range dirtyDirs {
			if entries, ok := pkgFiles[d]; ok {
				filtered[d] = entries
			}
		}
		pkgFiles = filtered
		// Rebuild dirs list for the filtered set (keep "." for repo summary).
		dirs = make([]string, 0, len(pkgFiles)+1)
		for d := range pkgFiles {
			dirs = append(dirs, d)
		}
		dirs = append(dirs, ".")
	}

	existingBatch, err := ix.Store.ExistingSHAsBatch(ctx, dirs)
	if err != nil {
		return 0, fmt.Errorf("existing SHAs: %w", err)
	}
	jobs, err := ix.planPackageJobs(ctx, startTime, pkgFiles, existingBatch)
	if err != nil {
		return 0, err
	}
	generated, err := ix.runPackageJobs(ctx, startTime, jobs)
	if err != nil {
		return generated, err
	}
	repoGen, err := ix.cascadeRepoSummary(ctx, startTime, existingBatch)
	return generated + repoGen, err
}

// groupSummariesByDir bins each (path, sha) by its directory and
// returns the dir list (including "." for the repo summary) plus the
// per-dir entries.
func groupSummariesByDir(allSHAs map[string]string) ([]string, map[string][]pkgFileEntry) {
	pkgFiles := make(map[string][]pkgFileEntry)
	for path, sha := range allSHAs {
		dir := filepath.Dir(path)
		pkgFiles[dir] = append(pkgFiles[dir], pkgFileEntry{path, sha})
	}
	dirs := make([]string, 0, len(pkgFiles)+1)
	for d := range pkgFiles {
		dirs = append(dirs, d)
	}
	dirs = append(dirs, ".")
	return dirs, pkgFiles
}

type pkgFileEntry struct {
	path string
	sha  string
}

// planPackageJobs computes the per-dir pkgSHA and either touches the
// existing package_summary row (cache hit) or queues a regeneration
// job (cache miss).
func (ix *Indexer) planPackageJobs(
	ctx context.Context,
	startTime time.Time,
	pkgFiles map[string][]pkgFileEntry,
	existingBatch map[string]map[string]bool,
) ([]pkgJob, error) {
	var jobs []pkgJob
	for dir, entries := range pkgFiles {
		if ctx.Err() != nil {
			break
		}
		// Drop test-file entries so the package summary describes the
		// production surface, not the test suite. Test-only dirs fall
		// back to all entries so they still get a summary. Mirror of
		// the filter in Run()'s Pass 5.
		summarized := entries
		prod := entries[:0:0]
		for _, e := range entries {
			if !ignore.IsTestPath(e.path) {
				prod = append(prod, e)
			}
		}
		if len(prod) > 0 {
			summarized = prod
		}
		shas := make([]string, len(summarized))
		filePaths := make([]string, len(summarized))
		for i, e := range summarized {
			shas[i] = e.sha
			filePaths[i] = e.path
		}
		sort.Strings(shas)
		pkgSHA := chunkSHA(packageSummaryPromptVersion + "\x00" + strings.Join(shas, ":"))
		if existingBatch[dir][pkgSHA] {
			if err := ix.Store.TouchSeen(ctx, dir, pkgSHA, "", 0, 0, startTime); err != nil {
				return nil, err
			}
			continue
		}
		jobs = append(jobs, pkgJob{dir: dir, filePaths: filePaths, pkgSHA: pkgSHA})
	}
	return jobs, nil
}

// runPackageJobs fans out chat summarization across SummaryConcurrency
// workers. Each worker commits its package_summary durably the moment it
// is generated (see commitPackageSummary) rather than collecting all
// results into a single all-or-nothing final batch.
//
// This is what makes the background cascade resumable. The watcher
// cancels the idle context on any fresh fs event (watch.markDirty), so in
// an active repo the cascade is routinely preempted mid-flight. With a
// batch-at-the-end commit, a preemption lost every summary the chat
// endpoint had already produced and the cascade never converged (dex
// #33). Committing per package means completed packages persist; combined
// with planPackageJobs's cache-hit skip, the cascade finishes across the
// watcher's later flush→idle retries. The expensive chat call still
// honours ctx and aborts promptly — only the cheap commit is protected.
//
// Concurrent UpsertMany is safe by design: the store opens with
// _busy_timeout and serializes first-write dim init (store.Store).
func (ix *Indexer) runPackageJobs(ctx context.Context, startTime time.Time, jobs []pkgJob) (int, error) {
	if len(jobs) == 0 {
		return 0, nil
	}
	conc := ix.Options.SummaryConcurrency
	if conc < 1 {
		conc = 1
	}
	modPath := readModulePath(ix.Proj.Root)
	var generated atomic.Int64
	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(conc)
	for i := range jobs {
		j := jobs[i]
		eg.Go(func() error {
			fileSummaries, err := ix.Store.FileSummariesForPaths(egctx, j.filePaths)
			if err != nil || len(fileSummaries) == 0 {
				return nil
			}
			grounding := ix.fetchPackageGrounding(egctx, j.dir, modPath)
			summary, err := summarizePackage(egctx, ix.Options.Chat, ix.Options.SummaryModels.Package, j.dir, fileSummaries, grounding)
			if err != nil {
				ix.drainLog.Warn("package summarize failed", "dir", j.dir, "err", err)
				return nil
			}
			if strings.TrimSpace(summary) == "" {
				return nil
			}
			if err := ix.commitPackageSummary(ctx, j.dir, j.pkgSHA, summary, startTime); err != nil {
				ix.drainLog.Warn("commit package_summary failed", "dir", j.dir, "err", err)
				return nil
			}
			generated.Add(1)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return int(generated.Load()), err
	}
	return int(generated.Load()), nil
}

// commitPackageSummary embeds one package_summary, upserts it, and GCs
// older-SHA rows for the same dir. The commit runs on a
// cancellation-detached context (context.WithoutCancel) so neither a
// sibling worker's failure nor an idle-context cancellation can discard a
// summary the chat endpoint already produced — the unit of durable
// progress is one package, not the whole cascade (dex #33). The embed +
// upsert here are cheap relative to the chat call that produced `summary`,
// so finishing them after a cancel costs little and buys convergence.
func (ix *Indexer) commitPackageSummary(ctx context.Context, dir, pkgSHA, summary string, startTime time.Time) error {
	cctx := context.WithoutCancel(ctx)
	c := chunk.Chunk{Path: dir, Kind: chunk.KindPackageSummary, Content: summary}
	vecs, err := ix.Embed.Embed(cctx, []string{c.EmbedText()})
	if err != nil {
		return fmt.Errorf("package embed: %w", err)
	}
	row := store.PendingChunk{
		Path:       dir,
		Kind:       chunk.KindPackageSummary,
		ContentSHA: pkgSHA,
		Content:    summary,
		Vec:        vecs[0],
	}
	if err := ix.Store.UpsertMany(cctx, []store.PendingChunk{row}, startTime); err != nil {
		return err
	}
	if _, err := ix.Store.DeleteOtherSummariesForPath(cctx, dir, chunk.KindPackageSummary, pkgSHA); err != nil {
		ix.drainLog.Warn("gc stale package_summary failed", "path", dir, "err", err)
	}
	return nil
}

// repoSummaryMaxPackages caps how many package summaries feed
// summarizeRepo. Empirical: a 14-package input to qwen2.5-coder:7b
// produces sensible synthesis; 40 inputs causes the model to abandon
// synthesis and start describing one package as if it were the whole
// repo. 15 sits in the safe zone — small enough for the 7B model to
// reason about, large enough that the top-by-centrality cut still
// captures architecturally significant packages. Tune up only after
// switching to a larger summary model.
const repoSummaryMaxPackages = 15

// repoSummaryPromptVersion is included in the repo_summary cache key
// so a prompt change naturally invalidates every cached repo_summary
// on the next cascade. Bump when summarizeRepo's prompt changes shape
// in a way that should re-run on already-summarized projects.
//
// v4 — added the graph-grounded PACKAGES section + grounding rule.
const repoSummaryPromptVersion = "v4"

// packageSummaryPromptVersion mirrors repoSummaryPromptVersion for the
// per-package summaries. Folded into pkgSHA at both Pass 5 and the
// drainer so a prompt iteration re-runs `summarizePackage` on the
// next index regardless of whether file SHAs changed. Bump when
// summarizePackage's prompt or grounding shape changes.
//
// v1 — initial version, accompanies the graph-grounded prompt rollout
// (EXPORTED SYMBOLS + PROJECT IMPORTS sections).
const packageSummaryPromptVersion = "v1"

// topRepoSummaryInput loads package summaries, sorts them by
// PackageCentrality DESC, and returns the top-N (capped at
// repoSummaryMaxPackages). Falls back to unsorted input if the
// centrality query fails — the summary is best-effort enrichment, not
// worth blocking on. Returns nil slices when no package summaries
// exist yet. The two returned slices are parallel: dirs[i] owns
// contents[i], so callers fetch graph grounding aligned with the
// summary order the model sees.
func (ix *Indexer) topRepoSummaryInput(ctx context.Context) (contents, dirs []string, err error) {
	pkgRows, err := ix.Store.SummariesByKindWithMeta(ctx, chunk.KindPackageSummary)
	if err != nil || len(pkgRows) == 0 {
		return nil, nil, err
	}
	centrality, cerr := ix.Store.PackageCentrality(ctx)
	if cerr == nil && centrality != nil {
		sort.SliceStable(pkgRows, func(i, j int) bool {
			ci, cj := centrality[pkgRows[i].Path], centrality[pkgRows[j].Path]
			if ci != cj {
				return ci > cj
			}
			return pkgRows[i].Path < pkgRows[j].Path
		})
	}
	if len(pkgRows) > repoSummaryMaxPackages {
		pkgRows = pkgRows[:repoSummaryMaxPackages]
	}
	if ix.Options.Verbose {
		paths := make([]string, len(pkgRows))
		for i, r := range pkgRows {
			paths[i] = r.Path
		}
		ix.drainLog.Info("repo summary input", "count", len(pkgRows), "paths", strings.Join(paths, ","))
	}
	contents = make([]string, len(pkgRows))
	dirs = make([]string, len(pkgRows))
	for i, r := range pkgRows {
		contents[i] = r.Content
		dirs[i] = r.Path
	}
	return contents, dirs, nil
}

// cascadeRepoSummary regenerates the repo_summary chunk from the
// current package_summary state, or touches an existing one on cache
// hit. Returns (0, nil) when the repo summary couldn't be produced
// (no package summaries yet, summarize call failed, etc.) so callers
// don't surface a hard error on what is best-effort enrichment.
func (ix *Indexer) cascadeRepoSummary(ctx context.Context, startTime time.Time, existingBatch map[string]map[string]bool) (int, error) {
	if ctx.Err() != nil {
		return 0, nil
	}
	pkgSummaries, pkgDirs, err := ix.topRepoSummaryInput(ctx)
	if err != nil || len(pkgSummaries) == 0 {
		return 0, nil
	}
	repoSHA := chunkSHA(repoSummaryPromptVersion + "\x00" + strings.Join(pkgSummaries, "\x00"))
	if existingBatch["."][repoSHA] {
		if err := ix.Store.TouchSeen(ctx, ".", repoSHA, "", 0, 0, startTime); err != nil {
			return 0, err
		}
		return 0, nil
	}
	grounding := ix.fetchRepoGrounding(ctx, pkgDirs)
	summary, err := summarizeRepo(ctx, ix.Options.Chat, ix.Options.SummaryModels.Repo, pkgSummaries, grounding)
	if err != nil {
		ix.drainLog.Warn("repo summarize failed", "err", err)
		return 0, nil
	}
	if strings.TrimSpace(summary) == "" {
		return 0, nil
	}
	vecs, err := ix.Embed.Embed(ctx, []string{chunk.KindRepoSummary + "\n" + summary})
	if err != nil {
		ix.drainLog.Warn("repo summary embed failed", "err", err)
		return 0, nil
	}
	rows := []store.PendingChunk{{
		Path:       ".",
		Kind:       chunk.KindRepoSummary,
		ContentSHA: repoSHA,
		Content:    summary,
		Vec:        vecs[0],
	}}
	if err := ix.Store.UpsertMany(ctx, rows, startTime); err != nil {
		return 0, err
	}
	if _, err := ix.Store.DeleteOtherSummariesForPath(ctx, ".", chunk.KindRepoSummary, repoSHA); err != nil {
		ix.drainLog.Warn("gc stale repo_summary failed", "err", err)
	}
	return 1, nil
}
