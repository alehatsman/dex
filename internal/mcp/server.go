// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/watch"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// AutoWatchConfig configures the MCP server's lazy per-project watcher.
// Zero value (Enabled=false) disables auto-watching entirely; tools
// behave exactly as before.
type AutoWatchConfig struct {
	// Enabled toggles the per-project watcher. When true, the first
	// MCP request that resolves a project also spawns a `watch.Watcher`
	// goroutine that lives for the server's lifetime — keeping the
	// chunk index fresh as files change and (when Summarize is set)
	// filling pending summaries in the background.
	Enabled bool
	// Debounce is the quiet window between fs events before re-indexing
	// (default 500ms).
	Debounce time.Duration
	// Summarize, when true and the parent Server's ChatClient is
	// configured, enables per-flush summary queueing + the idle drainer.
	// When false the watcher only keeps the chunk index fresh.
	Summarize bool
	// OnIdleAfter is the quiet window after a flush before the summary
	// drainer fires (default 5s). Ignored when Summarize is false.
	OnIdleAfter time.Duration
	// BatchSize bounds rows per idle drain (default 10).
	BatchSize int
	// IndexConcurrency caps Pass 1 worker count (default 0 = GOMAXPROCS).
	IndexConcurrency int
	// SummaryConcurrency caps in-flight chat calls during the drain.
	SummaryConcurrency int
	// ChunkSummaryMinLines forwards to index.Options.ChunkSummaryMinLines.
	ChunkSummaryMinLines int
	// YieldWindow forwards to index.Options.YieldWindow: the idle drainer
	// skips a tick if a foreground query ran within this window. 0 = off.
	YieldWindow time.Duration
	// Logger receives spawn/teardown messages; nil = io.Discard.
	Logger *slog.Logger
}

// Server holds everything the MCP handlers need.
type Server struct {
	EmbedClient    *embed.Client
	ChatClient     *chat.Client         // optional — when nil, view_summarize is not registered
	SummaryClient  *chat.Client         // optional — used by the auto-watcher's background drainer; falls back to ChatClient if nil
	SummaryModels  index.SummaryModels  // optional — per-tier model overrides forwarded to the auto-watcher's indexer
	RerankClient   rerank.HealthChecker // optional — only consulted by `status` for health reporting; the actual rerank wiring goes through StoreOpts.Reranker
	CompressClient *chat.Client         // optional — health reported by status
	DraftClient    *chat.Client         // optional — health reported by status
	IndexDir       string               // base dir holding per-project index folders
	StoreOpts      store.Options        // applied to every Store opened by the server
	AutoWatch      AutoWatchConfig      // lazy per-project watcher; zero value disables

	// runCtx is set at the start of RunStdio and is used as the parent
	// context for spawned watcher goroutines. nil for non-stdio usage
	// (CLI helpers that build a Server for a single call) — ensureWatcher
	// checks this and bails so one-shot CLI tools never leak goroutines.
	runCtx context.Context
	// watchers tracks per-project watcher spawns so each project gets
	// exactly one watcher across the server's lifetime. Keyed by
	// proj.Project.ID; value is *struct{} (presence-only).
	watchers sync.Map
	// watcherWG lets RunStdio wait for all watcher goroutines to drain
	// before returning.
	watcherWG sync.WaitGroup

	// searchThrottle tracks per-session repeated searches for the same
	// (query, project) pair. After 4 identical searches a hint is added;
	// after 7 a stronger warning fires. Resets after 5 minutes of idle.
	searchThrottle   sync.Map // key: string → *throttleEntry
	searchThrottleMu sync.Mutex

	// readCache tracks which files each MCP session has already received,
	// keyed by session ID then relative path. The value is the etag (content
	// hash) at the time of delivery. Used by view_summarize to return
	// status=unchanged on re-reads so the model can reuse context it already
	// has instead of receiving the full content again.
	readCache   map[string]map[string]string // sessionID → relPath → etag
	readCacheMu sync.Mutex
}

type throttleEntry struct {
	count  int
	lastAt time.Time
}

// readCacheCheck returns true when this session has previously received
// relPath at exactly this etag, meaning the model already has the content.
func (s *Server) readCacheCheck(sessionID, relPath, etag string) bool {
	if sessionID == "" {
		return false
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readCache == nil {
		return false
	}
	return s.readCache[sessionID][relPath] == etag
}

// readCacheMark records that sessionID has received relPath at etag.
func (s *Server) readCacheMark(sessionID, relPath, etag string) {
	if sessionID == "" {
		return
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readCache == nil {
		s.readCache = make(map[string]map[string]string)
	}
	if s.readCache[sessionID] == nil {
		s.readCache[sessionID] = make(map[string]string)
	}
	s.readCache[sessionID][relPath] = etag
}

// searchThrottleHint increments the repetition counter for (query, project)
// and returns a hint string when the pattern crosses a threshold. Returns ""
// on first few calls. Counters reset after 5 minutes of idle.
func (s *Server) searchThrottleHint(query, project string) string {
	const idleReset = 5 * time.Minute
	key := project + "\x00" + query
	now := time.Now()

	raw, _ := s.searchThrottle.LoadOrStore(key, &throttleEntry{})
	e := raw.(*throttleEntry)

	s.searchThrottleMu.Lock()
	if now.Sub(e.lastAt) > idleReset {
		e.count = 0
	}
	e.count++
	e.lastAt = now
	count := e.count
	s.searchThrottleMu.Unlock()

	switch {
	case count >= 7:
		return fmt.Sprintf("search_semantic called %d times with identical query — consider storing findings via knowledge action=add instead of re-searching.", count)
	case count >= 4:
		return fmt.Sprintf("repeated search (%d times) — if this keeps returning the same results, store key findings with the knowledge tool.", count)
	}
	return ""
}

// Search, FindSymbol, Related, Summarize are thin exported wrappers
// around the unexported MCP handlers so the CLI can reuse the same
// logic that the stdio server exposes over JSON-RPC. The MCP SDK
// passes a *sdk.CallToolRequest into every handler; CLI callers
// don't have one, so the wrappers pass nil.
func (s *Server) Search(ctx context.Context, in SearchInput) (SearchOutput, error) {
	_, out, err := s.search(ctx, nil, in)
	return out, err
}

func (s *Server) FindSymbol(ctx context.Context, in FindSymbolInput) (FindSymbolOutput, error) {
	_, out, err := s.findSymbol(ctx, nil, in)
	return out, err
}

func (s *Server) Related(ctx context.Context, in RelatedInput) (RelatedOutput, error) {
	_, out, err := s.related(ctx, nil, in)
	return out, err
}

func (s *Server) Summarize(ctx context.Context, in SummarizeInput) (SummarizeOutput, error) {
	_, out, err := s.summarize(ctx, nil, in)
	return out, err
}

func (s *Server) Status(ctx context.Context) (StatusOutput, error) {
	_, out, err := s.status(ctx, nil, StatusInput{})
	return out, err
}

func (s *Server) CompressOutput(ctx context.Context, in CompressInput) (CompressOutput, error) {
	_, out, err := s.compressOutput(ctx, nil, in)
	return out, err
}

func (s *Server) Knowledge(ctx context.Context, in KnowledgeInput) (KnowledgeOutput, error) {
	_, out, err := s.knowledge(ctx, nil, in)
	return out, err
}

func (s *Server) Session(ctx context.Context, in SessionInput) (SessionOutput, error) {
	_, out, err := s.session(ctx, nil, in)
	return out, err
}

// ─── tool: search_semantic ────────────────────────────────────────────────

type SearchInput struct {
	Query       string   `json:"query" jsonschema:"natural-language or code query"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip (e.g. ['vendor/', 'internal/legacy/'])"`
}

type SearchHit struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	// Score is the cosine similarity in [-1, 1]. Always populated.
	Score float32 `json:"score"`
	// BM25Score is the lexical (FTS5) score when the hit surfaced
	// through the BM25 leg of hybrid search. Larger = better. Zero
	// for semantic-only hits.
	BM25Score float32 `json:"bm25_score,omitempty"`
	// RRFScore is the fused rank used for ordering when hybrid search
	// is active. Zero when search ran semantic-only.
	RRFScore float32 `json:"rrf_score,omitempty"`
	// RerankScore is the cross-encoder relevance score in [0, 1] when
	// rerank ran. Zero when no reranker was wired or it failed open.
	RerankScore float32 `json:"rerank_score,omitempty"`
	// Role is a compact tag describing how the symbol sits in the call
	// graph — e.g. "central:47/9pkg" (47 callers from 9 packages),
	// "leaf" (no callees), "exported-unused" (exported but no callers).
	// Empty when the symbol has no graph node (non-Go file, top-level
	// const, etc.) or sits in the unremarkable middle. See formatRole.
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type SearchOutput struct {
	Status   string      `json:"status"`             // "ok" | "no-index" | "embedding-service-unreachable" | "error"
	Hint     string      `json:"hint,omitempty"`     // human-readable suggestion for the model
	Endpoint string      `json:"endpoint,omitempty"` // when unreachable
	Project  string      `json:"project,omitempty"`  // resolved project root
	Stale    bool        `json:"stale,omitempty"`    // last_indexed older than 24h
	Hits     []SearchHit `json:"hits,omitempty"`
}

// resolveProject canonicalizes projectRoot (falling back to cwd) and
// resolves it to a Project. On failure it returns a non-empty hint that
// callers can surface as a Status:"error" response.
//
// Side effect: on successful resolution under stdio mode, ensures a
// per-project watcher goroutine is running (no-op if AutoWatch is
// disabled or one is already spawned).
func (s *Server) resolveProject(projectRoot string) (*proj.Project, string) {
	root := projectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, "could not determine project root; pass project_root explicitly"
		}
		root = wd
	}
	p, err := proj.Resolve(root, s.IndexDir)
	if err != nil {
		return nil, fmt.Sprintf("resolve project: %v", err)
	}
	s.markForeground(p)
	s.ensureWatcher(p)
	return p, ""
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

// ensureWatcher lazily spawns a Watcher goroutine for this project,
// at most once per server lifetime. No-op unless RunStdio set runCtx
// (i.e. only the stdio MCP path opts in) AND AutoWatch.Enabled is
// true. Concurrency-safe; the goroutine self-cleans when runCtx ends.
func (s *Server) ensureWatcher(p *proj.Project) {
	if s == nil || s.runCtx == nil || s.runCtx.Err() != nil {
		return
	}
	if !s.AutoWatch.Enabled {
		return
	}
	if _, loaded := s.watchers.LoadOrStore(p.ID, struct{}{}); loaded {
		return
	}
	s.watcherWG.Add(1)
	go s.runWatcher(p)
}

// runWatcher owns the lifecycle of a single project's Watcher inside
// the MCP server. Closes its store + ignores when the goroutine
// returns so RunStdio's defer s.watcherWG.Wait() drains cleanly.
func (s *Server) runWatcher(p *proj.Project) {
	defer s.watcherWG.Done()
	// On exit, free the slot — if the server is shutting down nothing
	// reads it again; if a future request hits the same project after
	// a watcher errored out, we can respawn.
	defer s.watchers.Delete(p.ID)

	logger := s.AutoWatch.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	if err := proj.CheckIndexable(p, false); err != nil {
		logger.Info("mcp watch: skipping (not indexable)", "root", p.Root, "err", err)
		return
	}
	if err := p.EnsureCacheDir(); err != nil {
		logger.Warn("mcp watch: cache dir failed", "root", p.Root, "err", err)
		return
	}
	st, err := store.OpenWith(s.runCtx, p.DBPath, s.StoreOpts)
	if err != nil {
		logger.Warn("mcp watch: store open failed", "root", p.Root, "err", err)
		return
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		logger.Warn("mcp watch: ignore init failed", "root", p.Root, "err", err)
		return
	}

	summaryChat := s.SummaryClient
	if summaryChat == nil {
		summaryChat = s.ChatClient
	}
	ixOpts := index.Options{
		Logger:      logger,
		Concurrency: s.AutoWatch.IndexConcurrency,
	}
	if s.AutoWatch.Summarize && summaryChat != nil {
		ixOpts.Summarize = true
		ixOpts.DeferSummaries = true
		ixOpts.Chat = summaryChat
		ixOpts.SummaryModels = s.SummaryModels
		ixOpts.SummaryConcurrency = s.AutoWatch.SummaryConcurrency
		ixOpts.ChunkSummaryMinLines = s.AutoWatch.ChunkSummaryMinLines
		ixOpts.YieldWindow = s.AutoWatch.YieldWindow
	}
	ix := index.New(p, st, s.EmbedClient, ig, ixOpts)

	wOpts := watch.Options{
		Debounce: s.AutoWatch.Debounce,
		Logger:   logger,
	}
	if ixOpts.Summarize {
		wOpts.OnIdle = ix.IdleSummaryDrainer(s.AutoWatch.BatchSize)
		wOpts.OnIdleAfter = s.AutoWatch.OnIdleAfter
	}
	w := watch.New(ix, ig, p.Root, wOpts)
	logger.Info("mcp watch: starting", "root", p.Root, "summarize", ixOpts.Summarize)
	if err := w.Run(s.runCtx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Warn("mcp watch: exited with error", "root", p.Root, "err", err)
	}
}

func (s *Server) search(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	out := SearchOutput{}
	if strings.TrimSpace(in.Query) == "" {
		return nil, SearchOutput{Status: "error", Hint: "query is empty — pass a natural-language description or code fragment"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, SearchOutput{Status: "error", Hint: hint}, nil
	}
	out.Project = p.Root

	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Status = "no-index"
			out.Hint = fmt.Sprintf("no index for %s — run `dex index %s` first, then retry. Fall back to grep / Glob in the meantime.", p.Root, p.Root)
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = err.Error()
		return nil, out, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}

	em := s.EmbedClient
	vecs, err := em.Embed(ctx, []string{in.Query})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			out.Status = "embedding-service-unreachable"
			out.Endpoint = em.Endpoint()
			out.Hint = "the local embedding service is offline — fall back to grep / Glob / ripgrep for this query."
			return nil, out, nil
		}
		out.Status = "error"
		out.Hint = fmt.Sprintf("embed: %v", err)
		return nil, out, nil
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("open index: %v", err)
		return nil, out, nil
	}
	defer st.Close()

	stats, err := st.Stats(ctx)
	if err == nil && !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
		out.Stale = true
		out.Hint = fmt.Sprintf("index is %s old — results may be stale; run `dex index %s` to refresh.",
			time.Since(stats.LastIndex).Round(time.Hour), p.Root)
	}

	hits, err := st.Search(ctx, vecs[0], in.Query, k)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("search: %v", err)
		return nil, out, nil
	}

	// Symbol leg: extract identifier tokens from the query, look them up
	// by exact name, and RRF-fuse with the semantic results. Runs in the
	// same request with no extra embedding round-trip — FindSymbol is a
	// pure SQL index scan.
	idents := extractIdentifiers(in.Query)
	if len(idents) > 0 {
		symPool := k * 3
		if symPool < 15 {
			symPool = 15
		}
		if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
			hits = fuseWithSymbols(hits, symHits, k)
		}
	}

	// Graph-proximity lane: boost chunks from files graph-adjacent to
	// recently-touched session files. Silently skips when no session exists
	// or the graph hasn't been built — never fails the search.
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok && len(ss.Files) > 0 {
		seeds := make([]string, 0, len(ss.Files))
		for _, f := range ss.Files {
			seeds = append(seeds, f.Path)
		}
		if neighbors, err := st.GraphNeighborFiles(ctx, seeds, 15); err == nil && len(neighbors) > 0 {
			if graphHits, err := st.HitsForFiles(ctx, neighbors, k*2); err == nil && len(graphHits) > 0 {
				hits = fuseWithGraphNeighbors(hits, graphHits, k)
			}
		}
	}

	hits = rerankLocal(hits, len(idents) > 0)

	out.Status = "ok"
	if hint := s.searchThrottleHint(in.Query, p.Root); hint != "" {
		out.Hint = hint
	}
	for _, h := range hits {
		if excluded(h.Path, in.Exclude) {
			continue
		}
		out.Hits = append(out.Hits, SearchHit{
			Path:        h.Path,
			Kind:        h.Kind,
			StartLine:   h.StartLine,
			EndLine:     h.EndLine,
			Score:       h.Score,
			BM25Score:   h.BM25Score,
			RRFScore:    h.RRFScore,
			RerankScore: h.RerankScore,
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers),
			Content:     h.Content,
		})
	}
	return nil, out, nil
}

// collectSymbolHits runs FindSymbol for each identifier and returns a
// deduplicated hit list (keyed by path+start_line), in the order they
// surfaced across all identifier queries.
func collectSymbolHits(ctx context.Context, st *store.Store, idents []string, pool int) []store.Hit {
	seen := map[string]struct{}{}
	var out []store.Hit
	for _, id := range idents {
		bare := id
		if i := strings.LastIndex(bare, "."); i >= 0 {
			bare = bare[i+1:]
		}
		hits, err := st.FindSymbol(ctx, bare, pool)
		if err != nil {
			continue
		}
		for _, h := range hits {
			key := h.Path + ":" + strconv.Itoa(h.StartLine)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, h)
			if len(out) >= pool {
				return out
			}
		}
	}
	return out
}

// fuseWithSymbols merges a semantic hit list with a symbol hit list via
// Reciprocal Rank Fusion (k=60) and returns the top-n results. The
// dedup key is (path, start_line). Semantic hits already carry Score /
// BM25Score / RRFScore from the store; symbol-only hits get Score=1.0
// (exact-match signal). The new RRFScore field reflects the cross-lane
// fused rank for all returned hits.
func fuseWithSymbols(semantic, symbol []store.Hit, n int) []store.Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(semantic)+len(symbol))
	byKey := make(map[hitKey]store.Hit, len(semantic)+len(symbol))

	for i, h := range semantic {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
	}
	for i, h := range symbol {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		if _, exists := byKey[hk]; !exists {
			h.Score = 1.0 // exact name match
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}

	out := make([]store.Hit, 0, len(all))
	for _, r := range all {
		h := byKey[r.key]
		h.RRFScore = r.score
		out = append(out, h)
	}
	return out
}

// fuseWithGraphNeighbors merges semantic+symbol hits with graph-proximity
// hits via Reciprocal Rank Fusion (k=60). Graph hits represent chunks from
// files that are graph-adjacent to recently-touched session files, so they
// carry structural relevance even when lexically or semantically distant.
// The graph lane is weighted at 0.5× to avoid drowning out direct matches.
func fuseWithGraphNeighbors(primary, graphHits []store.Hit, n int) []store.Hit {
	const kRRF = 60
	type hitKey struct {
		path string
		line int
	}
	scores := make(map[hitKey]float32, len(primary)+len(graphHits))
	byKey := make(map[hitKey]store.Hit, len(primary)+len(graphHits))

	for i, h := range primary {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 1.0 / float32(kRRF+i+1)
		byKey[hk] = h
	}
	for i, h := range graphHits {
		hk := hitKey{h.Path, h.StartLine}
		scores[hk] += 0.5 / float32(kRRF+i+1)
		if _, exists := byKey[hk]; !exists {
			byKey[hk] = h
		}
	}

	type ranked struct {
		key   hitKey
		score float32
	}
	all := make([]ranked, 0, len(scores))
	for hk, s := range scores {
		all = append(all, ranked{hk, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}

	out := make([]store.Hit, 0, len(all))
	for _, r := range all {
		h := byKey[r.key]
		h.RRFScore = r.score
		out = append(out, h)
	}
	return out
}

// excluded returns true when path matches any entry in the exclude list.
// An exclude entry matches if path equals it or path has it as a prefix
// (treating it as a directory prefix).
func excluded(path string, exclude []string) bool {
	for _, ex := range exclude {
		if ex == "" {
			continue
		}
		if path == ex || strings.HasPrefix(path, ex) {
			return true
		}
	}
	return false
}

// rerankLocal applies post-RRF quality signals before returning search results.
// Passes: (1) noise penalties + definition boost, (2) file coherence boost,
// (3) MMR-style diversity decay. Operates on RRFScore; falls back to cosine
// Score when RRF didn't run (semantic-only search).
func rerankLocal(hits []store.Hit, isSymbolQuery bool) []store.Hit {
	if len(hits) == 0 {
		return hits
	}
	for i := range hits {
		if hits[i].RRFScore == 0 {
			hits[i].RRFScore = hits[i].Score
		}
	}

	// Pass 1: per-hit signals.
	for i := range hits {
		if isNoisePath(hits[i].Path) {
			hits[i].RRFScore *= 0.3
		}
		if isSymbolQuery && isDefinitionKind(hits[i].Kind) {
			hits[i].RRFScore *= 1.5
		}
	}

	// Pass 2: file coherence — boost all chunks from files with ≥2 hits.
	fileCnt := make(map[string]int, len(hits))
	for _, h := range hits {
		fileCnt[h.Path]++
	}
	for i := range hits {
		if fileCnt[hits[i].Path] >= 2 {
			hits[i].RRFScore *= 1.15
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].RRFScore > hits[j].RRFScore })

	// Pass 3: MMR diversity — decay chunks beyond the 2nd from the same file.
	seen := make(map[string]int, len(hits))
	for i := range hits {
		seen[hits[i].Path]++
		for excess := seen[hits[i].Path] - 2; excess > 0; excess-- {
			hits[i].RRFScore *= 0.7
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].RRFScore > hits[j].RRFScore })
	return hits
}

// isNoisePath returns true for test files, examples, fixtures, and demo
// directories that should rank lower in general code searches.
func isNoisePath(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(path, "_test.") ||
		strings.Contains(path, "/testdata/") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(path, "/example") ||
		strings.Contains(path, "/demo/") ||
		strings.Contains(path, "/fixture")
}

// isDefinitionKind returns true for tree-sitter node types that represent
// a declaration site (function, method, struct, class, interface, type).
func isDefinitionKind(kind string) bool {
	if strings.HasSuffix(kind, ":window") {
		return false
	}
	return strings.Contains(kind, "function") ||
		strings.Contains(kind, "method") ||
		strings.Contains(kind, "class") ||
		strings.Contains(kind, "struct") ||
		strings.Contains(kind, "interface") ||
		strings.Contains(kind, "type_decl") ||
		strings.Contains(kind, "impl_item")
}

// ─── tool: search_symbol ──────────────────────────────────────────────────

type FindSymbolInput struct {
	Name        string `json:"name" jsonschema:"exact identifier name to look up (case-sensitive, e.g. 'MyFunc', 'HTTPHandler')"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"max results to return (default 10)"`
}

type FindSymbolOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "not-found" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits,omitempty"`
}

func (s *Server) findSymbol(ctx context.Context, _ *sdk.CallToolRequest, in FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, FindSymbolOutput{Status: "error", Hint: "name is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, FindSymbolOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, FindSymbolOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer st.Close()
	hits, err := st.FindSymbol(ctx, in.Name, in.K)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("search_symbol: %v", err)}, nil
	}
	out := FindSymbolOutput{Status: "ok", Project: p.Root}
	if len(hits) == 0 {
		out.Status = "not-found"
		hint := fmt.Sprintf("no chunk with name=%q in the index; check spelling or re-index if recently added.", in.Name)
		// Near-miss surface: substring matches give the agent something
		// real to retry with instead of guessing. Errors are non-fatal —
		// the original "not-found" hint is still useful on its own.
		if cands, candErr := st.FindSymbolCandidates(ctx, in.Name, 5); candErr == nil && len(cands) > 0 {
			hint += " Did you mean: " + strings.Join(cands, ", ") + "?"
		}
		out.Hint = hint
		return nil, out, nil
	}
	for _, h := range hits {
		out.Hits = append(out.Hits, SearchHit{
			Path:      h.Path,
			Kind:      h.Kind,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Score:     1.0,
			Role:      formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers),
			Content:   h.Content,
		})
	}
	return nil, out, nil
}

// ─── tool: graph_neighbors ────────────────────────────────────────────────

type RelatedInput struct {
	Path        string `json:"path" jsonschema:"relative file path of the source chunk (e.g. 'internal/store/store.go')"`
	StartLine   int    `json:"start_line" jsonschema:"start line of the source chunk (1-indexed)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int    `json:"k,omitempty" jsonschema:"number of related chunks to return (default 8, max 30)"`
}

type RelatedOutput struct {
	Status  string      `json:"status"` // "ok" | "no-index" | "not-found" | "embedding-service-unreachable" | "error"
	Hint    string      `json:"hint,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits,omitempty"`
}

func (s *Server) related(ctx context.Context, _ *sdk.CallToolRequest, in RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	if strings.TrimSpace(in.Path) == "" {
		return nil, RelatedOutput{Status: "error", Hint: "path is empty"}, nil
	}
	if in.StartLine <= 0 {
		return nil, RelatedOutput{Status: "error", Hint: "start_line must be ≥ 1"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, RelatedOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, RelatedOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer st.Close()
	hits, err := st.RelatedChunks(ctx, in.Path, in.StartLine, k)
	if err != nil {
		if strings.Contains(err.Error(), "no chunk at") {
			return nil, RelatedOutput{Status: "not-found", Project: p.Root,
				Hint: err.Error() + " — check that path and start_line match an indexed chunk exactly."}, nil
		}
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("related: %v", err)}, nil
	}
	out := RelatedOutput{Status: "ok", Project: p.Root}
	for _, h := range hits {
		out.Hits = append(out.Hits, SearchHit{
			Path:      h.Path,
			Kind:      h.Kind,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Score:     h.Score,
			Content:   h.Content,
		})
	}
	return nil, out, nil
}

// ─── tool: view_summarize ─────────────────────────────────────────────────

type SummarizeInput struct {
	Path        string   `json:"path" jsonschema:"file path to summarize; relative paths are resolved against project_root"`
	Paths       []string `json:"paths,omitempty" jsonschema:"batch mode: list of files (max 10); all use the same mode; path is ignored when paths is non-empty"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Mode        string   `json:"mode,omitempty" jsonschema:"read fidelity: 'full' (default, summarize via LLM), 'signatures' (indexed symbols + source lines, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM)"`
	StartLine   int      `json:"start_line,omitempty" jsonschema:"first line to summarize (1-indexed, inclusive); 0 = beginning of file"`
	EndLine     int      `json:"end_line,omitempty" jsonschema:"last line to summarize (1-indexed, inclusive); 0 = end of file"`
	Focus       string   `json:"focus,omitempty" jsonschema:"optional steering — e.g. 'public API surface', 'side effects', 'error handling'"`
	Temperature float32  `json:"temperature,omitempty" jsonschema:"sampling temperature (0 = server default)"`
	MaxTokens   int      `json:"max_tokens,omitempty" jsonschema:"maximum tokens to generate (0 = server default)"`
	Etag        string   `json:"etag,omitempty" jsonschema:"content hash from a prior read; if the file is unchanged the server returns status=unchanged — re-use the content already in context instead of re-reading"`
}

type SummarizeOutput struct {
	Status       string   `json:"status"` // "ok" | "unchanged" | "chat-service-unreachable" | "error"
	Hint         string   `json:"hint,omitempty"`
	Project      string   `json:"project,omitempty"`
	Path         string   `json:"path,omitempty"` // resolved path, relative to project root
	Paths        []string `json:"paths,omitempty"`
	StartLine    int      `json:"start_line,omitempty"`
	EndLine      int      `json:"end_line,omitempty"`
	Bytes        int      `json:"bytes,omitempty"`     // how many bytes were sent to the model
	Truncated    bool     `json:"truncated,omitempty"` // true if the slice was cut to fit the cap
	Model        string   `json:"model,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Content      string   `json:"content,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Etag         string   `json:"etag,omitempty"` // sha256[:16] of file content; pass back on re-reads
}

// maxSummarizeBytes caps the slice we send to the chat endpoint. Above
// this the local model's quality drops sharply and latency spikes;
// callers wanting a whole-repo overview should use ask_codebase with
// RAG instead. Tuned to fit comfortably in a 32B-coder context window
// alongside the system prompt and the summary itself.
const maxSummarizeBytes = 64 * 1024

func (s *Server) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	out := SummarizeOutput{}

	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "full"
	}
	isFull := mode == "full"

	if isFull && s.ChatClient == nil {
		return nil, SummarizeOutput{Status: "error", Hint: "chat client not configured on this server"}, nil
	}
	if len(in.Paths) > 0 {
		return s.summarizeBatch(ctx, in)
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, SummarizeOutput{Status: "error", Hint: "path is empty"}, nil
	}
	root := in.ProjectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: "could not determine project root; pass project_root explicitly"}, nil
		}
		root = wd
	}
	p, err := proj.Resolve(root, s.IndexDir)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("resolve project: %v", err)}, nil
	}
	s.markForeground(p)
	out.Project = p.Root
	if isFull {
		out.Endpoint = s.ChatClient.Endpoint()
		out.Model = s.ChatClient.ModelName()
	}

	// Resolve path under the project root. Reject anything that
	// escapes it (so an MCP caller can't read /etc/passwd by passing
	// "/etc/passwd" or "../../etc/passwd").
	target := in.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(p.Root, target)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file does not exist: %s", target)}, nil
		}
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("resolve path: %v", err)}, nil
	}
	relTarget, err := filepath.Rel(p.Root, realTarget)
	if err != nil || strings.HasPrefix(relTarget, "..") || relTarget == ".." {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("path %s is outside project root %s", target, p.Root)}, nil
	}
	fi, err := os.Stat(realTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("stat: %v", err)}, nil
	}
	if fi.IsDir() {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s is a directory — pass a file path", relTarget)}, nil
	}
	out.Path = relTarget

	data, err := os.ReadFile(realTarget)
	if err != nil {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("read: %v", err)}, nil
	}

	h := sha256.Sum256(data)
	etag := hex.EncodeToString(h[:])[:16]

	var sessionID string
	if req != nil && req.Session != nil {
		sessionID = req.Session.ID()
	}
	if in.Etag != "" && in.Etag == etag && s.readCacheCheck(sessionID, relTarget, etag) {
		return nil, SummarizeOutput{Status: "unchanged", Project: out.Project, Path: relTarget, Etag: etag}, nil
	}

	switch {
	case strings.HasPrefix(mode, "lines:"):
		rest := strings.TrimPrefix(mode, "lines:")
		start, end, ok := parseLinesRange(rest)
		if !ok {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("invalid lines mode %q — expected lines:N-M (e.g. lines:10-40)", in.Mode)}, nil
		}
		slice, sliceStart, sliceEnd := sliceLines(data, start, end)
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		out.Bytes = len(slice)
		out.Status = "ok"
		out.Etag = etag
		out.Content = string(slice)
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil

	case mode == "signatures":
		st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		defer st.Close()
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		if len(syms) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		content := formatSignatures(data, syms, relTarget)
		if related := graphRelatedHint(ctx, st, relTarget); related != "" {
			content += related
		}
		// N16: inline best task-relevant symbol body when a session task is declared.
		content = inlineTaskSymbol(ctx, st, data, syms, content)
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil

	case mode == "map":
		// N14: non-code files get a pure-Go structural outline; no index needed.
		if content, ok := nonCodeMap(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Content = content
			out.Bytes = len(content)
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
		st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		defer st.Close()
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		imports, err := st.ImportsForFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("import query: %v", err)}, nil
		}
		if len(syms) == 0 && len(imports) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed data for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		content := formatMap(relTarget, syms, imports)
		if related := graphRelatedHint(ctx, st, relTarget); related != "" {
			content += related
		}
		// N16: inline best task-relevant symbol body when a session task is declared.
		if len(syms) > 0 {
			content = inlineTaskSymbol(ctx, st, data, syms, content)
		}
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil

	default: // full
		slice, sliceStart, sliceEnd := sliceLines(data, in.StartLine, in.EndLine)
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		if len(slice) > maxSummarizeBytes {
			slice = slice[:maxSummarizeBytes]
			out.Truncated = true
		}
		out.Bytes = len(slice)

		if lineCount := bytes.Count(data, []byte("\n")) + 1; lineCount > 250 {
			out.Hint = fmt.Sprintf("⚠ Large file (%d lines): pass mode=signatures or mode=map to reduce tokens.", lineCount)
		}

		system := buildSummarizeSystem(in.Focus)
		userContent := fmt.Sprintf("FILE: %s (lines %d-%d)\n\n```\n%s\n```",
			relTarget, sliceStart, sliceEnd, slice)

		resp, err := s.ChatClient.Generate(ctx, []chat.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		}, chat.Options{
			Temperature: in.Temperature,
			MaxTokens:   in.MaxTokens,
		})
		if err != nil {
			if errors.Is(err, chat.ErrUnreachable) {
				out.Status = "chat-service-unreachable"
				out.Hint = "the local chat-completion service is offline."
				return nil, out, nil
			}
			out.Status = "error"
			out.Hint = fmt.Sprintf("chat: %v", err)
			return nil, out, nil
		}

		out.Status = "ok"
		out.Etag = etag
		out.Content = resp.Content
		out.FinishReason = resp.FinishReason
		if resp.Model != "" {
			out.Model = resp.Model
		}
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil
	}
}

// summarizeBatch handles file_view when paths[] is provided.
// All files are processed with the same mode in a single call.
func (s *Server) summarizeBatch(ctx context.Context, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	const maxBatch = 10
	if len(in.Paths) > maxBatch {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("batch too large: max %d files per call, got %d", maxBatch, len(in.Paths))}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "signatures"
	}
	var sb strings.Builder
	var resolvedPaths []string
	var project string
	for _, rawPath := range in.Paths {
		single := in
		single.Path = rawPath
		single.Paths = nil
		_, out, err := s.summarize(ctx, nil, single)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s: %v", rawPath, err)}, nil
		}
		if project == "" {
			project = out.Project
		}
		if out.Status != "ok" {
			fmt.Fprintf(&sb, "## %s\n⚠ %s\n\n", rawPath, out.Hint)
			continue
		}
		fmt.Fprintf(&sb, "## %s\n%s\n\n", out.Path, out.Content)
		resolvedPaths = append(resolvedPaths, out.Path)
	}
	return nil, SummarizeOutput{
		Status:  "ok",
		Project: project,
		Content: strings.TrimRight(sb.String(), "\n"),
		Paths:   resolvedPaths,
	}, nil
}

// parseLinesRange parses "N-M" from a lines:N-M mode string.
func parseLinesRange(s string) (start, end int, ok bool) {
	i := strings.IndexByte(s, '-')
	if i <= 0 {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(s[:i])
	m, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || n < 1 || m < n {
		return 0, 0, false
	}
	return n, m, true
}

// graphRelatedHint returns a compact "Related (call graph): ..." line
// listing files graph-adjacent to relPath, or "" when the graph is absent
// or has no neighbors. Never fails — graph errors are silently swallowed.
func graphRelatedHint(ctx context.Context, st *store.Store, relPath string) string {
	neighbors, err := st.GraphNeighborFiles(ctx, []string{relPath}, 8)
	if err != nil || len(neighbors) == 0 {
		return ""
	}
	return "\n# Related (call graph): " + strings.Join(neighbors, ", ") + "\n"
}

// sigWindow is the max additional lines past start_line included as a
// symbol's signature. Covers multi-line Go/Python/TS declarations
// without pulling in the body.
const sigWindow = 4

// formatSignatures produces a compact listing of indexed symbols with
// their opening signature lines extracted from src.
//
// Output order: types/structs/interfaces first (stable declarations that
// LLM providers can prefix-cache), then functions/methods (volatile bodies).
// Within each group, source order is preserved.
func formatSignatures(src []byte, syms []store.GraphSymbol, relPath string) string {
	lines := bytes.Split(bytes.TrimRight(src, "\n"), []byte("\n"))
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s (%d symbols)\n\n", relPath, len(syms))

	// Emit type declarations first (struct, interface, type), then callables.
	isTypeKind := func(kind string) bool {
		return kind == "struct" || kind == "interface" || kind == "type"
	}
	writeSym := func(sym store.GraphSymbol) {
		si := sym.StartLine - 1
		if si < 0 || si >= len(lines) {
			return
		}
		ei := si + sigWindow
		if e := sym.EndLine - 1; e < ei {
			ei = e
		}
		if ei >= len(lines) {
			ei = len(lines) - 1
		}
		fmt.Fprintf(&b, "%s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		for i := si; i <= ei; i++ {
			b.Write(lines[i])
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	for _, sym := range syms {
		if isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	for _, sym := range syms {
		if !isTypeKind(sym.Kind) {
			writeSym(sym)
		}
	}
	return b.String()
}

// formatMap produces a compact dependency map for a file: its package-level
// imports and exported declarations, sourced from the index (no LLM, no file
// read). Unexported symbols are omitted so the output mirrors the public API.
func formatMap(relPath string, syms []store.GraphSymbol, imports []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FILE: %s\n\n", relPath)
	if len(imports) > 0 {
		b.WriteString("IMPORTS:\n")
		for _, imp := range imports {
			fmt.Fprintf(&b, "  %s\n", imp)
		}
		b.WriteByte('\n')
	}
	var exportedLines strings.Builder
	count := 0
	for _, sym := range syms {
		if len(sym.Name) == 0 || sym.Name[0] < 'A' || sym.Name[0] > 'Z' {
			continue
		}
		fmt.Fprintf(&exportedLines, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		count++
	}
	if count > 0 {
		fmt.Fprintf(&b, "EXPORTS (%d):\n", count)
		b.WriteString(exportedLines.String())
	}
	return b.String()
}

// sliceLines returns the byte slice of `data` between lines start and
// end (both 1-indexed, inclusive). Zero values mean "from start of
// file" / "to end of file". Returned start/end are clamped to the
// actual file extents so the caller can echo back what was used.
func sliceLines(data []byte, start, end int) ([]byte, int, int) {
	if start <= 0 && end <= 0 {
		return data, 1, chunk.LineCount(data)
	}
	if start <= 0 {
		start = 1
	}
	// Walk newlines once. Cheap and avoids splitting the whole file.
	var (
		startByte = -1
		endByte   = len(data)
		line      = 1
	)
	if start == 1 {
		startByte = 0
	}
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line++
		if startByte < 0 && line == start {
			startByte = i + 1
		}
		if end > 0 && line > end {
			endByte = i + 1
			break
		}
	}
	if startByte < 0 {
		// `start` is past EOF — return empty slice but record extents.
		return nil, start, start - 1
	}
	if end <= 0 || end > line {
		end = line
	}
	return data[startByte:endByte], start, end
}

func buildSummarizeSystem(focus string) string {
	base := "You are a file summarizer. Given a single file (or slice), produce a tight, factual summary the reader can use as a substitute for opening the file. " +
		"Lead with one sentence on what the file is for. Then a short bulleted list of the central items the file defines or exposes — picking the framing that fits the file kind: " +
		"exported types/functions for source code, targets and variables for Makefiles, top-level keys for config (YAML/TOML/JSON), section headings for docs, etc. " +
		"Also note key invariants, side effects, or constraints, and any non-obvious dependencies or cross-references. " +
		"Quote identifiers and names verbatim. No prose padding, no apologies, no restating the prompt. " +
		"Keep under 200 words. For trivial files (license, .gitignore, simple stubs) a single sentence is fine."
	if strings.TrimSpace(focus) != "" {
		base += " Focus specifically on: " + strings.TrimSpace(focus) + "."
	}
	return base
}

// ─── tool: index_status ───────────────────────────────────────────────────

type StatusInput struct{}

// BreakerStatus mirrors rerank.BreakerState in the index_status JSON.
// Surfaced under StatusOutput.RerankBreaker so operators can see when
// the breaker is open (and until when) without grepping logs.
type BreakerStatus struct {
	Open             bool   `json:"open"`
	OpenUntil        string `json:"open_until,omitempty"`
	ConsecutiveFails int    `json:"consecutive_fails,omitempty"`
}

type ProjectStatus struct {
	ID                            string `json:"id"`
	Root                          string `json:"root,omitempty"`
	Chunks                        int    `json:"chunks"`
	Files                         int    `json:"files"`
	Dim                           int    `json:"dim"`
	EmbedModel                    string `json:"embed_model,omitempty"`
	LastIndexed                   string `json:"last_indexed,omitempty"`
	PendingSummaries              int    `json:"pending_summaries,omitempty"`
	PendingSummariesOldestAgeSecs int    `json:"pending_summaries_oldest_age_s,omitempty"`
	QueueHint                     string `json:"queue_hint,omitempty"`
	LastSummarized                string `json:"last_summarized,omitempty"`
}

type StatusOutput struct {
	Endpoint          string          `json:"endpoint"`
	Reachable         bool            `json:"reachable"`
	Model             string          `json:"model"`
	ChatEndpoint      string          `json:"chat_endpoint,omitempty"`
	ChatReachable     bool            `json:"chat_reachable,omitempty"`
	ChatModel         string          `json:"chat_model,omitempty"`
	RerankEndpoint    string          `json:"rerank_endpoint,omitempty"`
	RerankReachable   bool            `json:"rerank_reachable,omitempty"`
	RerankModel       string          `json:"rerank_model,omitempty"`
	RerankBreaker     *BreakerStatus  `json:"rerank_breaker,omitempty"`
	CompressEndpoint  string          `json:"compress_endpoint,omitempty"`
	CompressReachable bool            `json:"compress_reachable,omitempty"`
	CompressModel     string          `json:"compress_model,omitempty"`
	DraftEndpoint     string          `json:"draft_endpoint,omitempty"`
	DraftReachable    bool            `json:"draft_reachable,omitempty"`
	DraftModel        string          `json:"draft_model,omitempty"`
	OllamaEndpoint    string          `json:"ollama_endpoint,omitempty"`
	OllamaEmbedModels []string        `json:"ollama_embed_models,omitempty"`
	OllamaChatModels  []string        `json:"ollama_chat_models,omitempty"`
	Version           string          `json:"version"`
	IndexDir          string          `json:"index_dir"`
	Projects          []ProjectStatus `json:"projects,omitempty"`
	Error             string          `json:"error,omitempty"`
}

// healthChecker abstracts a client that can report reachability.
type healthChecker interface {
	Health(ctx context.Context) error
}

func (s *Server) status(ctx context.Context, _ *sdk.CallToolRequest, _ StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	out := StatusOutput{
		Endpoint: s.EmbedClient.Endpoint(),
		Model:    s.EmbedClient.ModelName(),
		Version:  Version,
		IndexDir: s.IndexDir,
	}

	// Populate optional endpoint metadata before probing (read-only).
	if s.ChatClient != nil {
		out.ChatEndpoint = s.ChatClient.Endpoint()
		out.ChatModel = s.ChatClient.ModelName()
	}
	if s.RerankClient != nil {
		out.RerankEndpoint = s.RerankClient.Endpoint()
		out.RerankModel = s.RerankClient.ModelName()
		// Surface circuit-breaker state if the rerank client is wrapped.
		// A type assertion avoids leaking rerank.Breaker into the server's
		// signature; callers that wire a bare client (no breaker) skip this.
		if br, ok := s.RerankClient.(interface{ State() rerank.BreakerState }); ok {
			st := br.State()
			bs := &BreakerStatus{Open: st.Open, ConsecutiveFails: st.ConsecutiveFails}
			if !st.OpenUntil.IsZero() {
				bs.OpenUntil = st.OpenUntil.Format(time.RFC3339)
			}
			out.RerankBreaker = bs
		}
	}
	if s.CompressClient != nil {
		out.CompressEndpoint = s.CompressClient.Endpoint()
		out.CompressModel = s.CompressClient.ModelName()
	}
	if s.DraftClient != nil {
		out.DraftEndpoint = s.DraftClient.Endpoint()
		out.DraftModel = s.DraftClient.ModelName()
	}

	// Probe all clients concurrently — each has a 3 s timeout; running
	// them in parallel keeps the total wall-clock cost at ~3 s instead
	// of up to 15 s when clients are unreachable.
	probe := func(wg *sync.WaitGroup, client healthChecker, setResult func(bool, string)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := client.Health(pctx); err != nil {
				setResult(false, err.Error())
			} else {
				setResult(true, "")
			}
		}()
	}

	var wg sync.WaitGroup
	probe(&wg, s.EmbedClient, func(ok bool, errMsg string) {
		out.Reachable = ok
		out.Error = errMsg
	})
	if s.ChatClient != nil {
		probe(&wg, s.ChatClient, func(ok bool, _ string) { out.ChatReachable = ok })
	}
	if s.RerankClient != nil {
		probe(&wg, s.RerankClient, func(ok bool, _ string) { out.RerankReachable = ok })
	}
	if s.CompressClient != nil {
		probe(&wg, s.CompressClient, func(ok bool, _ string) { out.CompressReachable = ok })
	}
	if s.DraftClient != nil {
		probe(&wg, s.DraftClient, func(ok bool, _ string) { out.DraftReachable = ok })
	}
	wg.Go(func() {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if scan, ok := embed.ScanOllama(pctx); ok {
			out.OllamaEndpoint = scan.URL
			out.OllamaEmbedModels = scan.EmbedModels
			out.OllamaChatModels = scan.ChatModels
		}
	})
	wg.Wait()

	if entries, err := os.ReadDir(s.IndexDir); err == nil {
		type result struct {
			ps ProjectStatus
			ok bool
		}
		results := make([]result, len(entries))
		sem := make(chan struct{}, 8)
		var pwg sync.WaitGroup
		for i, e := range entries {
			if !e.IsDir() {
				continue
			}
			dbPath := filepath.Join(s.IndexDir, e.Name(), "index.db")
			if _, err := os.Stat(dbPath); err != nil {
				continue
			}
			pwg.Add(1)
			go func(idx int, id, path string) {
				defer pwg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				st, err := store.OpenWith(ctx, path, s.StoreOpts)
				if err != nil {
					return
				}
				stats, _ := st.Stats(ctx)
				root, _ := st.ProjectRoot(ctx)
				st.Close()
				ps := ProjectStatus{
					ID:               id,
					Root:             root,
					Chunks:           stats.Chunks,
					Files:            stats.Files,
					Dim:              stats.Dim,
					EmbedModel:       stats.EmbedModel,
					PendingSummaries: stats.PendingSummaries,
				}
				if stats.PendingSummariesOldestAge > 0 {
					ps.PendingSummariesOldestAgeSecs = int(stats.PendingSummariesOldestAge / time.Second)
				}
				// One-line hint when the queue looks stuck. Threshold tracks
				// the doc note: > 100 rows, or oldest > 1h, both suggest the
				// drainer isn't keeping up.
				if stats.PendingSummaries > 100 || stats.PendingSummariesOldestAge > time.Hour {
					ps.QueueHint = "summarization queue is behind; run `dex index summarize <path>`"
				}
				if !stats.LastIndex.IsZero() {
					ps.LastIndexed = stats.LastIndex.Format(time.RFC3339)
				}
				if !stats.LastSummarized.IsZero() {
					ps.LastSummarized = stats.LastSummarized.Format(time.RFC3339)
				}
				results[idx] = result{ps: ps, ok: true}
			}(i, e.Name(), dbPath)
		}
		pwg.Wait()
		for _, r := range results {
			if r.ok {
				out.Projects = append(out.Projects, r.ps)
			}
		}
	}
	return nil, out, nil
}

// RunStdio starts the MCP server bound to stdin/stdout. Sets runCtx
// so per-project Watcher goroutines spawned during the session share
// this ctx and exit cleanly when it ends. Blocks until ctx is
// cancelled or the transport closes, then waits for any spawned
// watchers to drain.
func (s *Server) RunStdio(ctx context.Context) error {
	s.runCtx = ctx
	defer s.watcherWG.Wait()

	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "dex",
		Version: Version,
	}, nil)

	registerTools(srv, s, toolTierFromEnv(), s.ChatClient != nil)

	return srv.Run(ctx, &sdk.StdioTransport{})
}

// toolSurface is the set of tool handlers registerTools wires onto an MCP
// server. *Server implements it against a local on-disk index; the remote
// shim's *remoteClient (remote.go) implements it by proxying each call to a
// `dex serve` REST endpoint. Funnelling both through one registerTools means
// the stdio and remote surfaces can never drift in tool names, JSON schemas,
// or descriptions — the schema for each tool is derived by the SDK from the
// shared Input type, so both backends advertise byte-identical tools.
type toolSurface interface {
	contextRouter(context.Context, *sdk.CallToolRequest, ContextInput) (*sdk.CallToolResult, ContextOutput, error)
	search(context.Context, *sdk.CallToolRequest, SearchInput) (*sdk.CallToolResult, SearchOutput, error)
	findSymbol(context.Context, *sdk.CallToolRequest, FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error)
	related(context.Context, *sdk.CallToolRequest, RelatedInput) (*sdk.CallToolResult, RelatedOutput, error)
	graphDeps(context.Context, *sdk.CallToolRequest, GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error)
	graphCallers(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphCallees(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphImpact(context.Context, *sdk.CallToolRequest, ImpactInput) (*sdk.CallToolResult, ImpactOutput, error)
	graphLinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphBacklinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphTags(context.Context, *sdk.CallToolRequest, TagInput) (*sdk.CallToolResult, TagOutput, error)
	overview(context.Context, *sdk.CallToolRequest, OverviewInput) (*sdk.CallToolResult, OverviewOutput, error)
	smells(context.Context, *sdk.CallToolRequest, SmellsInput) (*sdk.CallToolResult, SmellsOutput, error)
	routes(context.Context, *sdk.CallToolRequest, RoutesInput) (*sdk.CallToolResult, RoutesOutput, error)
	searchTree(context.Context, *sdk.CallToolRequest, SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error)
	knowledge(context.Context, *sdk.CallToolRequest, KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error)
	session(context.Context, *sdk.CallToolRequest, SessionInput) (*sdk.CallToolResult, SessionOutput, error)
	compressOutput(context.Context, *sdk.CallToolRequest, CompressInput) (*sdk.CallToolResult, CompressOutput, error)
	status(context.Context, *sdk.CallToolRequest, StatusInput) (*sdk.CallToolResult, StatusOutput, error)
	summarize(context.Context, *sdk.CallToolRequest, SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error)
	compose(context.Context, *sdk.CallToolRequest, ComposeInput) (*sdk.CallToolResult, ComposeOutput, error)
	specVerify(context.Context, *sdk.CallToolRequest, SpecVerifyInput) (*sdk.CallToolResult, SpecVerifyOutput, error)
}

// toolTier controls how many tools are exposed to MCP clients.
//
//   - TierAsk      — ask only (minimal, escape-hatch)
//   - TierStandard — ask + orientation + memory tools (default)
//   - TierPower    — everything (old DEX_EXPOSE_RAW_TOOLS=1)
type toolTier int

const (
	TierAsk      toolTier = iota // ask only
	TierStandard                 // ask + overview + session + knowledge + file_tree + search_context + file_view
	TierPower                    // full raw surface: search_*, graph_*, code_smells, graph_routes, compress_output, status
)

// toolTierFromEnv reads DEX_TOOLS (ask|standard|power). DEX_EXPOSE_RAW_TOOLS=1
// is honoured as a backward-compatible alias for power. Default: standard.
func toolTierFromEnv() toolTier {
	if exposeRawTools() {
		return TierPower
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_TOOLS"))) {
	case "power":
		return TierPower
	case "ask":
		return TierAsk
	default:
		return TierStandard
	}
}

// registerTools wires the dex tool surface onto srv, dispatching to h.
//
// Tiers (DEX_TOOLS env var, default: standard):
//   - TierAsk      → ask only
//   - TierStandard → ask + overview + session + knowledge + file_tree + search_context + file_view (if chat)
//   - TierPower    → everything above plus the raw search/graph/analysis tools
//
// DEX_EXPOSE_RAW_TOOLS=1 is honoured as a backward-compatible alias for power.
// The `dex` CLI subcommands are unaffected by tier.
func registerTools(srv *sdk.Server, h toolSurface, tier toolTier, chatAvailable bool) {
	// Power-only: raw search / graph / analysis lanes. Useful for CLI parity,
	// A-B debugging, and power users — too noisy for everyday agents.
	if tier >= TierPower {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "search_semantic",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Prefer `ask` for general code-understanding questions — it composes this " +
				"tool with symbol lookup and graph expansion. Use search_semantic directly only when you specifically " +
				"want raw ranking without intent routing. " +
				"Embeds the query and returns top-k matching chunks. Identifier tokens in the query (CamelCase, " +
				"snake_case, qualified names) are automatically looked up by exact symbol name and fused into the " +
				"results via Reciprocal Rank Fusion — no separate search_symbol call needed. " +
				"Supports exclude list to skip paths. " +
				"On error, returns a structured status: 'no-index' (run dex index first), " +
				"'embedding-service-unreachable' (fall back to grep), or 'ok'.",
		}, h.search)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "search_symbol",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Prefer `ask` — it detects identifiers in your question and runs this " +
				"lookup automatically as part of a fused response. Use search_symbol directly only when you " +
				"already have the exact identifier name and want nothing else. " +
				"Fast SQL lookup — no embedding required. Returns 'not-found' when no chunk with that name exists.",
		}, h.findSymbol)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_neighbors",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Prefer `ask` — it includes neighborhood expansion as part of routing. " +
				"Use graph_neighbors directly only when you already have the exact (path, start_line) of a chunk " +
				"and want its cosine neighbors. " +
				"Finds code that is semantically related even without keyword overlap.",
		}, h.related)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_deps",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Return the `imports` edges for a file or package — the package the file belongs to, " +
				"and the list of packages it depends on. Sourced from the static graph (no embedding, no chat). " +
				"Pass `path` (relative file inside the project) OR `package` (full package path). " +
				"Returns 'no-index' / 'no-graph' / 'not-found' when the project, graph, or symbol is missing.",
		}, h.graphDeps)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_callers",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Return functions that CALL the given symbol, from the static graph's `calls` edges. " +
				"Go-only for now (Python/JS/Rust callers fall back to ripgrep via `ask`). " +
				"Accepts a bare name (`Foo`), a qualified method (`(*Server).RunStdio`), or a package-qualified " +
				"name (`mcp.NewServer`). Multiple matches are returned with their package paths so the agent can " +
				"disambiguate. Returns 'no-graph' when calls edges haven't been indexed yet.",
		}, h.graphCallers)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_callees",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Return functions that the given symbol CALLS, from the static graph's `calls` edges. " +
				"Go-only for now. Same name resolution as graph_callers. " +
				"Returns 'no-graph' when calls edges haven't been indexed yet.",
		}, h.graphCallees)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_links",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Return the markdown documents that the given doc LINKS TO — outgoing `links` " +
				"(inline `[text](other.md)`) and `wikilinks` (`[[Note]]`) edges from the doc graph. " +
				"Pass `doc` as a path relative to the project root (e.g. 'docs/spec.md'); a unique basename works too. " +
				"The reverse direction is graph_backlinks. Returns 'no-graph' when the markdown doc graph " +
				"hasn't been indexed yet.",
		}, h.graphLinks)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_backlinks",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Return the markdown documents that LINK TO the given doc — incoming `links`/`wikilinks` " +
				"edges (Obsidian-style backlinks). Same `doc` resolution as graph_links. Useful for " +
				"'what references this spec'. Returns 'no-graph' when the markdown doc graph hasn't been indexed yet.",
		}, h.graphBacklinks)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_impact",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Transitive blast-radius analysis. Given a symbol, follows `calls` edges " +
				"in the callers direction up to max_depth (default 3) and returns every reachable function " +
				"with its hop depth and PageRank. Depth 1 = direct callers; depth 2 = their callers; etc. " +
				"Use before editing a widely-called symbol to gauge the ripple. " +
				"Same name resolution as graph_callers. Returns 'no-graph' when calls edges haven't been indexed yet.",
		}, h.graphImpact)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_tags",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Query the markdown tag graph. Pass `tag` (a #tag without the #) to list the documents " +
				"carrying it, ranked by doc importance — tag-based clustering. Or pass `doc` to list the tags that " +
				"document carries. Returns 'no-graph' when the markdown doc graph hasn't been indexed yet.",
		}, h.graphTags)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "graph_routes",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Detect HTTP handlers, MCP tool registrations, and gRPC service implementations " +
				"from the call graph. Matches ServeHTTP implementations, handle*/serve*-named functions, " +
				"and callers of registration functions (Handle, HandleFunc, AddTool, RegisterService, etc.). " +
				"Returns each handler with its file location and the registration function that wires it in. " +
				"Requires a graph index (`dex index . --graph=only`).",
		}, h.routes)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "code_smells",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "AST-based code quality signals derived from the graph index — no LLM required. " +
				"Returns three categories: `long_functions` (bodies >= min_func_lines, default 80), " +
				"`dead_exports` (exported functions/methods with no indexed callers), and " +
				"`god_files` (files with >= min_file_symbols symbols, default 30). " +
				"Requires a graph index (`dex index . --graph=only`). Use before a PR or refactor to spot obvious structural issues.",
		}, h.smells)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "compress_output",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Compress raw shell command output before using it in your context. " +
				"Pass the full output and a command hint (e.g. 'go test', 'git log', 'npm install', 'cargo build', 'docker build'). " +
				"Strips progress spinners, download noise, and consecutive duplicates; for go test keeps only failures " +
				"and summaries; for git diffs >80 lines strips unchanged context lines. " +
				"Returns compressed text, original/output line counts, and saved_pct. " +
				"No project index required — pure text transformation.",
		}, h.compressOutput)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "status",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Report dex endpoint health and the list of indexed projects with their chunk counts and last-indexed times.",
		}, h.status)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "spec_verify",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Verify a spec file against the project's code index. Reads the spec's ## Checklist " +
				"items (or ## Behavior clauses as fallback), embeds each clause, retrieves the top-5 matching " +
				"code chunks, and — when a chat model is configured — asks the model to judge whether the code " +
				"implements the clause (pass/fail/unknown). Returns per-item verdicts with code citations " +
				"(path:line), pass/fail/unknown counts, and a drift flag if commits have landed on covered paths " +
				"since the spec's last_verified commit. " +
				"Pass no_judge:true to skip the LLM pass and get raw citations only. " +
				"Returns 'no-index' when the project hasn't been indexed yet.",
		}, h.specVerify)
	}

	// Standard+ tools: orientation and persistent memory. Exposed by default so
	// agents can accumulate project context without any configuration.
	if tier >= TierStandard {
		sdk.AddTool(srv, &sdk.Tool{
			Name:        "overview",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Task-relevant project map. Given a task description, ranks every indexed file by " +
				"semantic similarity to the task fused with graph centrality, and returns two buckets: " +
				"`context` (top-k most relevant files with line counts and suggested file_view mode) and " +
				"`distant` (all other indexed files). Use this as the first call in an unfamiliar codebase " +
				"to decide what to read before touching code. Cheaper than ask — returns file paths only, " +
				"no inlined content. Requires the embedding service.",
		}, h.overview)

		sdk.AddTool(srv, &sdk.Tool{
			Name: "knowledge",
			Description: "Manage persistent project knowledge — facts, patterns, and gotchas that survive " +
				"session resets and reconnects. Actions: add (store a fact with an archetype and confidence), " +
				"list (retrieve top-k facts ordered by salience), delete (remove a fact by id). " +
				"Archetypes: Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact. " +
				"High-salience facts (Architecture, Gotcha) are automatically injected into ask responses " +
				"as knowledge_facts. No embedding required.",
		}, h.knowledge)

		sdk.AddTool(srv, &sdk.Tool{
			Name: "session",
			Description: "Manage per-project session memory across tool calls. " +
				"Actions: set_task (declare what you're working on), add_note (record a finding or decision), " +
				"add_file (track a file you read/wrote), get (retrieve the current session state), " +
				"clear (reset the session). " +
				"Session state (task + notes + files) is surfaced in ask responses as session_task so you " +
				"don't lose context across reconnects. No embedding required.",
		}, h.session)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "file_tree",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "List indexed files under a directory path. Returns individual files within " +
				"`depth` directory levels (default 3) and aggregates deeper files into their parent dirs " +
				"(dirs shown with trailing / and a summed chunk count). " +
				"No embedding required — reads directly from the index. " +
				"Use for orientation in an unfamiliar codebase before calling ask or file_view. " +
				"Returns 'no-index' when the project hasn't been indexed yet.",
		}, h.searchTree)

		sdk.AddTool(srv, &sdk.Tool{
			Name:        "search_context",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: "Single-call alternative to the search → signatures → lines chain. " +
				"Embeds the query, finds the top-k most relevant files, returns their symbol signatures " +
				"and the body of the best-matching symbol — all in one round trip. " +
				"Use at task-start to orient quickly without 2–3 follow-up calls. " +
				"Requires the embedding service. Returns 'no-index' / 'embedding-service-unreachable' for graceful fallback.",
		}, h.compose)

		if chatAvailable {
			sdk.AddTool(srv, &sdk.Tool{
				Name:        "file_view",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: "Prefer `ask` first — its `suggested_reads` will name the file worth " +
					"summarizing. Use file_view directly only when you already know which file you need digested. " +
					"Sends the file slice directly to the chat model. Pass `focus` to steer (e.g. 'public API surface'). " +
					"Path must resolve inside project_root. Files larger than 64 KB are truncated. " +
					"Pass paths[] (up to 10) to read multiple files in one call — all use the same mode. " +
					"Re-read savings: every response includes `etag` (content hash). On re-reads pass that etag back; " +
					"if the file is unchanged the server returns status=unchanged — reuse the content already in context. " +
					"On error, returns 'chat-service-unreachable' or 'error'.",
			}, h.summarize)
		}
	}

	sdk.AddTool(srv, &sdk.Tool{
		Name:        "ask",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: "PRIMARY ENTRY POINT for code-understanding questions — and, by default, the ONLY dex tool you " +
			"need. Call this BEFORE Grep/Glob/Read fan-out. When a chat model is configured it returns `answer`: a " +
			"synthesized, citation-bearing prose response (`path:line`) grounded in the evidence below — read that " +
			"first. `answer_model` names the model that produced it. The answer is absent only when the chat leg is " +
			"unreachable, in which case fall back to the evidence bundle + `next_action`. " +
			"Given a free-text question (and optional intent override), it picks a strategy, composes semantic search " +
			"+ symbol lookup + graph expansion, and returns a compact bundle: `semantic_hits`, `symbols`, `suggested_reads` " +
			"(both lanes carry their CONTENTS inlined by default — no follow-up Read needed in the common case), a prose " +
			"`next_action` directive you can execute verbatim, and an `avoid` line telling you what NOT to do. Each " +
			"SymbolHit carries `signature` (declaration line) and `doc` (leading comment block) so you can see the API " +
			"without reading the body. `annotations` is a per-path map populated by intent: always-on entries include " +
			"sibling `tests` (foo.go ↔ foo_test.go) and `nearest_doc` (closest CLAUDE.md / doc.go / README.md walking " +
			"up); editing_context adds `last_commit` / `last_author` (git blame) and `owners` (CODEOWNERS); architecture " +
			"and editing_context add `build_tags` and `package`. `references` carries the `calls` graph edges for " +
			"callers/callees intents (Go-only; other languages fall back to a ripgrep usage list). Inline content " +
			"shares ONE per-intent byte pool across both lanes: targeted intents budget ~60 lines / 4 KB per range " +
			"and ~20 KB total; exploration intents (architecture, package_topology) widen to ~120 lines / 8 KB per " +
			"range and ~40 KB total. Suggested_reads (~2 targeted / ~5 exploration) are filled first as the curated " +
			"cut; semantic_hits use the remaining budget. A range that appears in both lanes is read once and " +
			"charged once. Oversize ranges arrive with `truncated: true` and the original line range, so the caller " +
			"can Read the rest if needed. Pass `no_inline: true` to omit content payloads when you already have the " +
			"files open. Intent is inferred automatically " +
			"(behavior_search/symbol_lookup/callers/callees/architecture/package_topology/editing_context) — pass `intent` " +
			"only to override. Returns 'no-index' / 'embedding-service-unreachable' for graceful fallback to grep.",
	}, h.contextRouter)

}

// exposeRawTools is kept for backward compatibility — DEX_EXPOSE_RAW_TOOLS=1
// (or true/on/yes) maps to TierPower in toolTierFromEnv.
func exposeRawTools() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_EXPOSE_RAW_TOOLS"))) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// Version is set at build time via -ldflags.
var Version = "dev"
