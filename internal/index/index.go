// Package index orchestrates the walk → chunk → embed → upsert pipeline.
package index

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/logx"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// Options controls one index run.
type Options struct {
	MaxFileSize int64        // skip files larger than this (bytes); 0 = 1 MB default
	Verbose     bool         // emit per-event log lines (skip/embedding/etc.)
	Logger      *slog.Logger // destination for log output; nil = io.Discard
	// Concurrency caps the number of parallel file readers / chunkers in
	// Pass 1. <=0 = runtime.GOMAXPROCS(0). The walker itself stays
	// single-threaded (directory IO is cheap and serializes well with
	// inline mtime fast-path UPDATEs); only the expensive per-file work —
	// ReadFile, binary/secret detection, tree-sitter parse — runs on
	// workers.
	Concurrency int
	// Progress, when non-nil, is called at key milestones so callers can
	// render a progress indicator. phase is one of "walk", "embed".
	// done/total are counts for that phase; total may be 0 when unknown.
	// Called from the goroutine driving each phase — not thread-safe with
	// itself, but callers that only write to a terminal are fine.
	Progress func(phase string, done, total int)
}

// Indexer is the entry point.
type Indexer struct {
	Proj    *proj.Project
	Store   *store.Store
	Embed   embed.Embedder
	Ignore  *ignore.Matcher
	Options Options
}

func New(p *proj.Project, st *store.Store, em embed.Embedder, ig *ignore.Matcher, opt Options) *Indexer {
	if opt.MaxFileSize <= 0 {
		opt.MaxFileSize = 1 << 20 // 1 MB
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opt.Logger = opt.Logger.With("subsystem", "indexer")
	return &Indexer{Proj: p, Store: st, Embed: em, Ignore: ig, Options: opt}
}

// pathTask is a single file dispatched from the walker to the worker pool.
type pathTask struct {
	rel  string
	path string
}

// Run walks the project, chunks new/changed files, embeds, and upserts.
// Files unchanged since the last index get their last_seen_at bumped but
// are not re-embedded. Stale rows (files removed) are pruned at the end.
//
// Mtime fast-path: if a file's mtime is strictly < the previous run's
// last_indexed_at, we know the content is identical to what we
// processed last time. We TouchPath() all of its chunks in one UPDATE
// and skip the read+parse+SHA work entirely — turning the no-change
// re-index from O(files × parse) into O(files × stat + 1 UPDATE).
// The comparison must be strict: filesystem mtimes are second-granular
// on many platforms, so a file edited in the same second the previous
// run started carries mtime == last_indexed_at. A `<=` test would skip
// it forever; `<` sends equal-mtime files down the slow path, where the
// SHA dedup re-confirms (cheaply) whether they actually changed (#439).
// slowFile holds one file that survived all walk-phase filters and needs
// SHA-based deduplication against the store.
type slowFile struct {
	rel    string
	data   []byte
	chunks []chunk.Chunk
}

func (ix *Indexer) Run(ctx context.Context) error {
	// Monotonic stamp/cutoff: strictly exceeds every previously stored
	// last_seen_at, so a backward wall-clock step between runs can't make
	// this run's prune cutoff smaller than a prior run's stamps and leave
	// deleted files' chunks un-pruned (dex #32). Read before upserting.
	startTime, err := ix.Store.SeenTime(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("seen time: %w", err)
	}

	// Publish an "indexing in progress" marker so concurrent readers (e.g. a
	// `dex serve` daemon on the same DB) can warn that results are partial
	// rather than serving a half-rebuilt index as authoritative (#531).
	defer ix.markIndexing(ctx, startTime)()

	ix.Options.Logger.Info("index: starting", "root", ix.Proj.Root)

	// Refuse a silent embed-model swap: two same-dim models produce
	// vectors in different latent spaces, so mixing them corrupts the
	// vec table without tripping the dim check. EnsureEmbedModel writes
	// the model identity on first run and rejects mismatched subsequent
	// runs with an actionable hint. BM25-only indexes use a sentinel.
	modelName := "bm25-only"
	if ix.Embed != nil {
		modelName = ix.Embed.ModelName()
	}
	if err := ix.Store.EnsureEmbedModel(ctx, modelName); err != nil {
		return err
	}

	prevStats, statsErr := ix.Store.Stats(ctx)
	var lastIndexed time.Time
	if statsErr == nil {
		lastIndexed = prevStats.LastIndex
	}

	// Pass 1: walk the tree.
	//
	// The walker (this goroutine) does only cheap work — directory
	// traversal, ignore checks, symlink/extension/size filters, and the
	// inline mtime fast-path (one SQL UPDATE per unchanged file).
	// Keeping the fast-path inline avoids contending workers on the
	// SQLite writer lock for what is already a serial bottleneck.
	//
	// Expensive per-file work — ReadFile, LooksBinary/LooksLikeSecret,
	// tree-sitter Chunks — is fanned out to a pool of workers sized by
	// Options.Concurrency (default GOMAXPROCS). Each worker holds its
	// own tree-sitter parser via chunk.Chunks() so they don't share
	// state. Order of slowFiles is non-deterministic; subsequent passes
	// don't depend on it.
	conc := ix.Options.Concurrency
	if conc <= 0 {
		conc = runtime.GOMAXPROCS(0)
	}
	pathCh := make(chan pathTask, conc*4)
	resultCh := make(chan slowFile, conc*4)
	var (
		skipped    atomic.Int64
		mtimeSkips atomic.Int64
		mtimeSeen  int // mtime fast-path chunk count (walk goroutine only, no sync needed)
	)

	var workers sync.WaitGroup
	for i := 0; i < conc; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ix.fileWorker(ctx, pathCh, resultCh, startTime, &skipped)
		}()
	}

	// Collector drains resultCh into slowFiles. Runs on its own
	// goroutine so the walker can keep pushing into pathCh without
	// deadlocking against a full resultCh.
	var slowFiles []slowFile
	var collector sync.WaitGroup
	collector.Add(1)
	go func() {
		defer collector.Done()
		for sf := range resultCh {
			slowFiles = append(slowFiles, sf)
		}
	}()

	walkErr := filepath.WalkDir(ix.Proj.Root, ix.walkVisit(ctx, lastIndexed, startTime, pathCh, &skipped, &mtimeSkips, &mtimeSeen))
	close(pathCh)
	workers.Wait()
	close(resultCh)
	collector.Wait()
	if walkErr != nil {
		return fmt.Errorf("walk: %w", walkErr)
	}
	ix.Options.Logger.Info("index: walk done",
		"slow_files", len(slowFiles),
		"mtime_fast_path", mtimeSkips.Load(),
		"skipped", skipped.Load(),
		logx.DurMS(time.Since(startTime)))
	if ix.Options.Progress != nil {
		total := len(slowFiles) + int(mtimeSkips.Load())
		ix.Options.Progress("walk", total, total)
	}

	// Pass 2: SHA dedup + build embed queue.
	toEmbed, slowSeen, err := ix.buildPending(ctx, slowFiles, startTime)
	if err != nil {
		return err
	}
	seen := mtimeSeen + slowSeen

	// Pass 3: embed and upsert.
	embedSkipped, err := ix.runEmbedPhase(ctx, toEmbed, startTime)
	if err != nil {
		return err
	}

	pruned, err := ix.Store.PruneUnseen(ctx, startTime)
	if err != nil {
		return err
	}
	if pruned > 0 {
		ix.Options.Logger.Info("index: pruned stale chunks", logx.Count(int(pruned)))
	}
	if err := ix.Store.SetLastIndexedAt(ctx, startTime); err != nil {
		return err
	}
	ix.Options.Logger.Info("index: done",
		logx.Phase("done"), "chunks_seen", seen,
		"files_fast_path", mtimeSkips.Load(),
		"embedded", len(toEmbed)-embedSkipped,
		"embed_skipped", embedSkipped,
		"pruned", pruned,
		"skipped", skipped.Load(),
		logx.DurMS(time.Since(startTime)))
	return nil
}

// fileWorker drains pathCh, reads each file, detects binary/secret content,
// parses chunks, and sends results to resultCh. One goroutine per worker slot.
func (ix *Indexer) fileWorker(ctx context.Context, pathCh <-chan pathTask, resultCh chan<- slowFile, startTime time.Time, skipped *atomic.Int64) {
	for task := range pathCh {
		// Honour cancellation between files: a long index over a
		// 10k-file repo would otherwise drain pathCh fully even
		// after Ctrl-C — workers keep reading queued tasks until
		// the channel closes.
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(task.path) //nolint:gosec // path comes from WalkDir under project root
		if err != nil {
			continue
		}
		if ignore.LooksBinary(data) {
			skipped.Add(1)
			continue
		}
		// Skip files whose content matches a known secret
		// pattern — but allow-list test fixtures, which
		// routinely embed fake credentials as inputs to
		// their own detection logic.
		if !ignore.IsTestPath(task.rel) && ignore.LooksLikeSecret(data) {
			ix.Options.Logger.Warn("skip (matches secret pattern)", "path", task.rel)
			skipped.Add(1)
			continue
		}
		chunks, err := chunk.Chunks(ctx, task.rel, data)
		if err != nil {
			if ix.Options.Verbose {
				ix.Options.Logger.Info("chunk error", "path", task.rel, "err", err)
			}
			// Touch existing chunks so they survive PruneUnseen.
			// Without this, a parse-error file loses its chunks on
			// the first run, then the mtime fast-path skips it on
			// every subsequent run (no chunks to touch → slow path
			// → parse fails again → permanent absence from index).
			_, _ = ix.Store.TouchPath(ctx, task.rel, startTime)
			continue
		}
		select {
		case resultCh <- slowFile{rel: task.rel, data: data, chunks: chunks}:
		case <-ctx.Done():
			return
		}
	}
}

// walkVisit returns the fs.WalkDirFunc used by Run's Pass 1. It handles
// directory pruning, ignore matching, symlink rejection, mtime fast-path
// (cheap chunks-seen bump), and queuing slow-path files onto pathCh.
// mtimeSeen is written from the walk goroutine only (no sync required).
func (ix *Indexer) walkVisit(
	ctx context.Context,
	lastIndexed, startTime time.Time,
	pathCh chan<- pathTask,
	skipped, mtimeSkips *atomic.Int64,
	mtimeSeen *int,
) fs.WalkDirFunc {
	return func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if ix.Options.Verbose {
				ix.Options.Logger.Info("walk error", "path", path, "err", err)
			}
			return nil
		}
		// Honour context cancellation between files — useful for Ctrl-C
		// in CLI mode and for shutdown in watch mode.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, _ := filepath.Rel(ix.Proj.Root, path)
		if rel == "." {
			return nil
		}
		if ix.Ignore.Match(rel, d.IsDir()) {
			if d.IsDir() {
				// If the user just added e.g. `node_modules/` to
				// `.gitignore` between runs, the chunks under that
				// directory must be evicted on this pass — otherwise
				// they'd live forever in the index because the
				// directory is no longer walked. Drop them by path
				// prefix; the walk continues skipping the subtree.
				_ = ix.Store.DeletePathPrefix(ctx, rel+"/")
				return filepath.SkipDir
			}
			// Newly-ignored single file: drop its chunks.
			_ = ix.Store.DeletePath(ctx, rel)
			return nil
		}
		if d.IsDir() {
			// Skip git worktree checkouts: they contain a .git FILE (not dir).
			// Regular source dirs never have a .git file.
			gitMarker := filepath.Join(path, ".git")
			if fi, err2 := os.Lstat(gitMarker); err2 == nil && !fi.IsDir() {
				_ = ix.Store.DeletePathPrefix(ctx, rel+"/")
				return filepath.SkipDir
			}
			return nil
		}
		// Skip symlinks. They risk (a) double-indexing the same content
		// under both the link path and the target path, and (b)
		// silently pulling content from outside the project root for
		// links into the file system. Operators who want symlinked
		// trees indexed should follow them at the shell level.
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
		if info.Size() > ix.Options.MaxFileSize {
			skipped.Add(1)
			if ix.Options.Verbose {
				ix.Options.Logger.Info("skip (too large)", "path", rel, "size", info.Size())
			}
			return nil
		}
		// Mtime fast-path: file hasn't changed since the last successful
		// index → just bump last_seen_at on its existing chunks
		// (TouchPath updates every chunk sharing `path = rel` in one shot).
		//
		if !lastIndexed.IsZero() && info.ModTime().Before(lastIndexed) {
			rows, terr := ix.Store.TouchPath(ctx, rel, startTime)
			if terr == nil && rows > 0 {
				*mtimeSeen += int(rows)
				mtimeSkips.Add(1)
				return nil
			}
			// rows==0: never indexed before, fall through to slow path.
		}
		select {
		case pathCh <- pathTask{rel: rel, path: path}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
}

// buildPending runs Pass 2: one batch SHA query for all slow-path files,
// deduplicates against the store, and returns the chunks that need embedding.
// seen is the total chunk count across all slow files (for the final log).
func (ix *Indexer) buildPending(ctx context.Context, slowFiles []slowFile, startTime time.Time) (toEmbed []pending, seen int, err error) {
	slowPaths := make([]string, len(slowFiles))
	for i, sf := range slowFiles {
		slowPaths[i] = sf.rel
	}
	existingBatch, err := ix.Store.ExistingSHAsBatch(ctx, slowPaths)
	if err != nil {
		return nil, 0, fmt.Errorf("existing SHAs: %w", err)
	}

	for _, sf := range slowFiles {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		existing := existingBatch[sf.rel]
		seen += len(sf.chunks)

		// content_sha1 is position-independent so a chunk that only shifts
		// lines is not re-embedded. That makes byte-identical chunks in one
		// file collide on UNIQUE(path, content_sha1) and silently drop all but
		// the last (#434). Disambiguate repeats with a per-file ordinal folded
		// into the SHA; the first occurrence keeps the plain content hash so
		// non-duplicate chunks (the overwhelming majority) are unaffected.
		dupCount := make(map[string]int, len(sf.chunks))
		for _, c := range sf.chunks {
			base := chunkSHA(c.Content)
			sha := dedupSHA(base, dupCount[base])
			dupCount[base]++
			if existing[sha] {
				if err := ix.Store.TouchSeen(ctx, sf.rel, sha, c.Name, c.StartLine, c.EndLine, startTime); err != nil {
					return nil, 0, err
				}
				continue
			}
			toEmbed = append(toEmbed, pending{rel: sf.rel, chunk: c, sha: sha})
		}
	}
	return toEmbed, seen, nil
}

// runEmbedPhase runs Pass 3: embed toEmbed in batches and upsert each batch.
// Returns the number of chunks skipped due to batch failures.
func (ix *Indexer) runEmbedPhase(ctx context.Context, toEmbed []pending, startTime time.Time) (embedSkipped int, err error) {
	if len(toEmbed) == 0 {
		return 0, nil
	}
	// Embed and upsert one batch at a time. If a later batch fails
	// (timeout, embedding service crash), earlier batches survive
	// in the store and the next index run skips them via
	// content-sha matching — no wasted GPU time on retry.
	batchSize := 32
	if ix.Embed != nil {
		if bs := ix.Embed.BatchSize(); bs > 0 {
			batchSize = bs
		}
	}
	if batchSize <= 0 {
		batchSize = 32
	}
	totalBatches := (len(toEmbed) + batchSize - 1) / batchSize
	ix.Options.Logger.Info("index: embedding",
		logx.Phase("embed"), "chunks", len(toEmbed),
		"batches", totalBatches,
		"batch_size", batchSize)
	var skippedBatches int
	var lastBatchErr error
	embedStart := time.Now()
	for start := 0; start < len(toEmbed); start += batchSize {
		// Bail between batches so Ctrl-C during a long embed pass
		// returns within one batch's wall-clock instead of waiting
		// for the http client's per-call timeout to fire.
		if err := ctx.Err(); err != nil {
			return embedSkipped, err
		}
		end := start + batchSize
		if end > len(toEmbed) {
			end = len(toEmbed)
		}
		batch := toEmbed[start:end]
		batchStart := time.Now()
		if err := ix.embedAndUpsertBatch(ctx, batch, startTime); err != nil {
			// A canceled/expired context is fatal: the operator asked to
			// stop (Ctrl-C) or the whole run is timing out — surface it,
			// don't mask it as a per-batch skip.
			if ctx.Err() != nil {
				return embedSkipped, err
			}
			// Isolate the failure: one bad batch (an embed error that
			// survived #436's retry, or a transient SQLite BUSY on upsert)
			// must not throw away every other batch's GPU work. Log, count,
			// and continue. These are new chunks (their row is created only
			// by the batch's UpsertMany), so a skip leaves no row behind —
			// nothing for PruneUnseen to act on — and the next run re-queues
			// them via content-sha matching. Same recoverable state as a
			// crash mid-embed, minus the lost work on the good batches.
			lastBatchErr = err
			skippedBatches++
			embedSkipped += len(batch)
			ix.Options.Logger.Warn("index: embed batch failed, skipping",
				"batch", start/batchSize+1,
				"batch_total", totalBatches,
				"chunks", len(batch),
				"err", err)
			if ix.Options.Progress != nil {
				ix.Options.Progress("embed", end, len(toEmbed))
			}
			continue
		}
		ix.Options.Logger.Info("index: embed batch",
			"batch", start/batchSize+1,
			"batch_total", totalBatches,
			"chunks", len(batch),
			logx.DurMS(time.Since(batchStart)))
		if ix.Options.Progress != nil {
			ix.Options.Progress("embed", end, len(toEmbed))
		}
	}
	// All batches failed → the backend is down, not a sporadic bad file.
	// Isolation must not silently turn a total outage into a "success"
	// that embedded nothing, so fail loud in that case.
	if skippedBatches > 0 && skippedBatches == totalBatches {
		return embedSkipped, fmt.Errorf("embed pass: all %d batch(es) failed; last error: %w", totalBatches, lastBatchErr)
	}
	if skippedBatches > 0 {
		ix.Options.Logger.Warn("index: embed pass completed with skipped batches",
			"skipped_batches", skippedBatches,
			"batch_total", totalBatches,
			"skipped_chunks", embedSkipped)
	}
	ix.Options.Logger.Info("index: embedding done",
		logx.DurMS(time.Since(embedStart)))
	return embedSkipped, nil
}

// markIndexing stamps the indexing-in-progress marker and returns a cleanup
// func (call via defer) that removes it. The clear runs on a fresh context
// so a cancelled run still unmarks; a crash leaves the marker for the
// staleness window to reap (#531).
func (ix *Indexer) markIndexing(ctx context.Context, startTime time.Time) func() {
	if serr := ix.Store.SetIndexing(ctx, startTime); serr != nil {
		ix.Options.Logger.Warn("index: could not set indexing marker", "err", serr)
	}
	return func() {
		clearCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cerr := ix.Store.ClearIndexing(clearCtx); cerr != nil {
			ix.Options.Logger.Warn("index: could not clear indexing marker", "err", cerr)
		}
	}
}

type pending struct {
	rel   string
	chunk chunk.Chunk
	sha   string
}

// embedAndUpsertBatch embeds one batch of pending chunks in a single
// Embed.Embed call and upserts them under startTime, driven by the
// post-pass batch embed loop.
func (ix *Indexer) embedAndUpsertBatch(ctx context.Context, batch []pending, startTime time.Time) error {
	rows := make([]store.PendingChunk, len(batch))
	for i, p := range batch {
		rows[i] = store.PendingChunk{
			Path:       p.rel,
			Kind:       p.chunk.Kind,
			Name:       p.chunk.Name,
			StartLine:  p.chunk.StartLine,
			EndLine:    p.chunk.EndLine,
			ContentSHA: p.sha,
			Content:    p.chunk.Content,
		}
	}
	if ix.Embed != nil {
		texts := make([]string, len(batch))
		for i, p := range batch {
			texts[i] = p.chunk.EmbedText()
		}
		vecs, err := ix.Embed.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("embed: server returned %d vectors for %d inputs", len(vecs), len(batch))
		}
		for i, v := range vecs {
			rows[i].Vec = v
		}
	}
	return ix.Store.UpsertMany(ctx, rows, startTime)
}

func chunkSHA(content string) string {
	h := sha1.Sum([]byte(content))
	return hex.EncodeToString(h[:])
}

// dedupSHA disambiguates byte-identical chunks within a single file. The first
// occurrence (ordinal 0) returns the plain content hash so the common case is
// unchanged; later occurrences mix the ordinal into the digest so each gets a
// distinct content_sha1 and survives UPSERT under UNIQUE(path, content_sha1).
func dedupSHA(base string, ordinal int) string {
	if ordinal == 0 {
		return base
	}
	h := sha1.Sum([]byte(base + "\x00" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(h[:])
}
