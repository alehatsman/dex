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
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graphrefresh"
	"github.com/alehatsman/dex/internal/heatmap"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/slo"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/watch"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerInstructions returns the MCP server instructions block that Claude Code
// receives at session init. It maps dex tools to their native equivalents so
// the agent uses them without being explicitly asked.
func ServerInstructions() string {
	return `dex is active — prefer its MCP tools over native equivalents:

Tool mapping (use these instead):
- ask(question)            instead of free-form reasoning about code structure
- map()                    instead of guessing layout — orient first in an unfamiliar repo
- find(query, path)        instead of Grep/rg for concept/intent searches
- trace(symbol, direction) instead of manual cross-ref tracing (callers/callees/path)
- impact(symbol)           instead of guessing an edit's blast radius
- read(file)               instead of Read for large files (signatures + summaries)
- shell(command)           instead of Bash for shell commands (compressed output)
- grep(pattern)            instead of rg for exact regex matches

Workflow:
1. Orient: ask(question) — routes intent, returns suggested_reads + next_action; map() for layout
2. Locate: find for concepts; trace to follow the call graph
3. Read: read for large files; native Read for small ones
4. Shell: shell(command) for build/test output

Power lanes (lookup, deps, callers, callees, path, diff, clusters, routes, smells, status, notes, session) are gated behind DEX_EXPERT — the verbs above cover everyday work.

Start every session by calling ask() with the task description.`
}

// AutoWatchConfig configures the MCP server's lazy per-project watcher.
// Zero value (Enabled=false) disables auto-watching entirely; tools
// behave exactly as before.
type AutoWatchConfig struct {
	// Enabled toggles the per-project watcher. When true, the first
	// MCP request that resolves a project also spawns a `watch.Watcher`
	// goroutine that lives for the server's lifetime — keeping the
	// chunk index fresh as files change.
	Enabled bool
	// Debounce is the quiet window between fs events before re-indexing
	// (default 500ms).
	Debounce time.Duration
	// IndexConcurrency caps Pass 1 worker count (default 0 = GOMAXPROCS).
	IndexConcurrency int
	// Logger receives spawn/teardown messages; nil = io.Discard.
	Logger *slog.Logger
}

// Server holds everything the MCP handlers need.
// watcherState tracks per-project watcher goroutines spawned by RunStdio.
type watcherState struct {
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
	// activityTracker accumulates per-project tool-call weights to surface
	// a knowledge-nudge hint when the agent has done significant work but
	// hasn't recorded any findings. Key: project root; value: *activityState.
	activityTracker sync.Map
}

// sessionState tracks per-MCP-session bookkeeping.
type sessionState struct {
	// sessionWG tracks in-flight sessionAutoFile goroutines so callers can
	// drain them. Tests use waitSessionWrites to avoid racing TempDir
	// cleanup against background store writes.
	sessionWG sync.WaitGroup
	// searchThrottle tracks per-session repeated searches for the same
	// (query, project) pair. After 4 identical searches a hint is added;
	// after 7 a stronger warning fires. Resets after 5 minutes of idle.
	searchThrottle   sync.Map // key: string → *throttleEntry
	searchThrottleMu sync.Mutex
	// bodyHandles stores @B<n> expansion handles issued by skeleton-mode reads.
	// Handles are scoped per session and expire when the session ends or the
	// server restarts. Key: "@B<n>"; value: the file location to expand.
	bodyHandles    map[string]map[string]bodyHandle // sessionID → "@B<n>" → handle
	bodyHandlesMu  sync.Mutex
	bodyHandlesSeq map[string]int // sessionID → next N
	// seen is the cross-turn dedup ledger (#344): per session, the turn on which
	// each locator (path:start-end) was first surfaced. A locator re-emitted on a
	// later turn is marked "seen" and its content omitted, so we never resend
	// bytes the agent already has. In-memory and connection-scoped — dies with the
	// session, no persistence or GC.
	seen   map[string]*seenState // sessionKey → ledger
	seenMu sync.Mutex
}

// cacheState holds in-memory caches scoped to the server lifetime.
type cacheState struct {
	// readCache tracks which files each MCP session has already received,
	// keyed by session ID then relative path. The value is the etag (content
	// hash) at the time of delivery. Used by view_summarize to return
	// status=unchanged on re-reads so the model can reuse context it already
	// has instead of receiving the full content again.
	readCache   map[string]map[string]string // sessionID → relPath → etag
	readCacheMu sync.Mutex
	// readContentCache stores the raw file bytes last delivered per (session,
	// path). Used by the delta re-read path (#217): when a file changes between
	// reads we diff the prior bytes against the new bytes and return a compact
	// unified diff when it is smaller than deltaThreshold × full content.
	readContentCache map[string]map[string][]byte // sessionID → relPath → raw bytes
	// answerCache is the bounded per-server cache for synthesized answers.
	answerCache answerCache
	// tgCache holds per-(root,prefix,ext) RAM-resident trigram indices used
	// by searchGrep to narrow candidate files before reading them.
	tgCache trigramCache
	// graphViewByPath caches the last loadGraphView result per project DB path.
	// Keyed by DBPath; value is *cachedGraphView. Invalidated when the graph's
	// max last_seen_at epoch changes.
	graphViewByPath sync.Map // string → *cachedGraphView
	// multiScaleByPath caches the last BuildMultiScale result per project DB path.
	// Keyed by DBPath; value is *cachedMultiScale. Invalidated when last_indexed_at changes.
	multiScaleByPath sync.Map // string → *cachedMultiScale
	// storeByPath caches an opened *store.Store per project DB path.
	// Keyed by DBPath; value is *cachedStore. The connection is opened once and
	// never closed per-request — SQLite WAL mode supports concurrent readers.
	storeByPath sync.Map // string → *cachedStore
}

// patternState detects anti-patterns (compression thrash, infinite loops).
type patternState struct {
	// bounce detects "compression thrash": same file re-requested within
	// bounceWindow after receiving a compressed view. shouldForceFull
	// returns true on the second request and clears the flag (single-use).
	bounce     *bounceTracker
	bounceOnce sync.Once
	// loop is the per-server loop detector. Lazily initialised by ld().
	loop     *loopDetector
	loopOnce sync.Once
}

type Server struct {
	EmbedClient  embed.Embedder
	ChatClient   chat.Chatter         // optional — when nil, view_summarize is not registered
	RerankClient rerank.HealthChecker // optional — only consulted by `status` for health reporting; the actual rerank wiring goes through StoreOpts.Reranker
	ExpandClient chat.Chatter         // optional — drives opt-in query-side expansion (#252); nil disables it
	ExpandMode   string               // server default expand level (off|on|full) when a request omits it
	IndexDir     string               // base dir holding per-project index folders
	StoreOpts    store.Options        // applied to every Store opened by the server
	AutoWatch    AutoWatchConfig      // lazy per-project watcher; zero value disables

	watcherState // project watcher goroutines
	sessionState // per-MCP-session tracking (throttle, dedup, body handles)
	cacheState   // in-memory read/content/answer caches
	patternState // loop and bounce-thrash detection
}

// sloFor returns the per-project SLO tracker. Config is loaded once from
// .dex/config.yml on first access; subsequent calls for the same root are
// served from the process-local registry in the slo package.
func (s *Server) sloFor(root string) *slo.Tracker {
	return slo.ForProject(root)
}

// sloAnnotation returns a non-empty annotation string when any warn/throttle
// violations are present; empty string when there are none or only block violations.
func sloAnnotation(vs []slo.Violation) string {
	var parts []string
	for _, v := range vs {
		if v.SLO.Action == slo.ActionBlock {
			continue
		}
		parts = append(parts, v.Annotation())
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// sloBlock returns the block message from the first block violation, or "".
func sloBlock(vs []slo.Violation) string {
	for _, v := range vs {
		if v.SLO.Action == slo.ActionBlock {
			return v.BlockMessage()
		}
	}
	return ""
}

type throttleEntry struct {
	count  int
	lastAt time.Time
}

type activityState struct {
	mu               sync.Mutex
	weightedScore    int
	significantCalls int // calls with weight >= 2
	lastKnowledgeAt  time.Time
	lastNudgeAt      time.Time
}

// activityRecord adds weight for a tool call. weight >= 2 counts as significant.
func (s *Server) bt() *bounceTracker {
	s.bounceOnce.Do(func() { s.bounce = newBounceTracker() })
	return s.bounce
}

func (s *Server) ld() *loopDetector {
	s.loopOnce.Do(func() { s.loop = newLoopDetector() })
	return s.loop
}

func (s *Server) activityRecord(project string, weight int) {
	raw, _ := s.activityTracker.LoadOrStore(project, &activityState{})
	a, ok := raw.(*activityState)
	if !ok {
		return
	}
	a.mu.Lock()
	a.weightedScore += weight
	if weight >= 2 {
		a.significantCalls++
	}
	a.mu.Unlock()
}

// activityKnowledgeRecorded resets the nudge clock when the agent stores a fact.
func (s *Server) activityKnowledgeRecorded(project string) {
	raw, _ := s.activityTracker.LoadOrStore(project, &activityState{})
	a, ok := raw.(*activityState)
	if !ok {
		return
	}
	a.mu.Lock()
	a.lastKnowledgeAt = time.Now()
	a.mu.Unlock()
}

// activityNudge returns a hint when the agent has done substantial work without
// recording findings. sessionTask makes the message context-sensitive. Returns ""
// when below threshold or the nudge was already shown recently.
func (s *Server) activityNudge(project, sessionTask string) string {
	const (
		scoreThreshold = 20
		callThreshold  = 5
		quietWindow    = 8 * time.Minute
	)
	raw, loaded := s.activityTracker.Load(project)
	if !loaded {
		return ""
	}
	a, ok := raw.(*activityState)
	if !ok {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.weightedScore < scoreThreshold || a.significantCalls < callThreshold {
		return ""
	}
	now := time.Now()
	if !a.lastKnowledgeAt.IsZero() && now.Sub(a.lastKnowledgeAt) < quietWindow {
		return ""
	}
	if !a.lastNudgeAt.IsZero() && now.Sub(a.lastNudgeAt) < quietWindow {
		return ""
	}
	a.lastNudgeAt = now
	if sessionTask != "" {
		return fmt.Sprintf("You've done significant work on %q — consider recording key findings with knowledge action=add so they survive a session reset.", sessionTask)
	}
	return "Significant activity detected — consider recording key findings with knowledge action=add so they survive a session reset."
}

// bodyHandle stores the file location of a skeleton-mode @B<n> expansion handle.
type bodyHandle struct {
	relPath   string
	startLine int
	endLine   int
	etag      string // file content hash at the time the handle was issued
}

// registerBodyHandles stores a slice of BodyEntry handles in the per-session
// map and returns the handle keys (@B1, @B2, …) so the caller can embed them
// in the skeleton output.
func (s *Server) registerBodyHandles(sessionID, relPath, etag string, bodies []compress.BodyEntry) {
	if sessionID == "" || len(bodies) == 0 {
		return
	}
	s.bodyHandlesMu.Lock()
	defer s.bodyHandlesMu.Unlock()
	if s.bodyHandles == nil {
		s.bodyHandles = make(map[string]map[string]bodyHandle)
		s.bodyHandlesSeq = make(map[string]int)
	}
	if s.bodyHandles[sessionID] == nil {
		s.bodyHandles[sessionID] = make(map[string]bodyHandle)
	}
	for _, be := range bodies {
		key := fmt.Sprintf("@B%d", be.N)
		s.bodyHandles[sessionID][key] = bodyHandle{
			relPath:   relPath,
			startLine: be.StartLine,
			endLine:   be.EndLine,
			etag:      etag,
		}
	}
}

// lookupBodyHandle returns the stored body handle for key (e.g. "@B3") in
// sessionID, and a boolean indicating whether it was found.
func (s *Server) lookupBodyHandle(sessionID, key string) (bodyHandle, bool) {
	s.bodyHandlesMu.Lock()
	defer s.bodyHandlesMu.Unlock()
	if s.bodyHandles == nil {
		return bodyHandle{}, false
	}
	h, ok := s.bodyHandles[sessionID][key]
	return h, ok
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

// readCacheGetContent returns the raw file bytes last delivered for (sessionID, relPath).
func (s *Server) readCacheGetContent(sessionID, relPath string) ([]byte, bool) {
	if sessionID == "" {
		return nil, false
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readContentCache == nil {
		return nil, false
	}
	b, ok := s.readContentCache[sessionID][relPath]
	return b, ok
}

// readCacheSetContent records the raw file bytes delivered for (sessionID, relPath).
func (s *Server) readCacheSetContent(sessionID, relPath string, data []byte) {
	if sessionID == "" {
		return
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readContentCache == nil {
		s.readContentCache = make(map[string]map[string][]byte)
	}
	if s.readContentCache[sessionID] == nil {
		s.readContentCache[sessionID] = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	s.readContentCache[sessionID][relPath] = cp
}

// sessionAutoFile records relPath in the active session (if one with a task
// exists) without blocking the caller. Safe to call from any mode.
func (s *Server) sessionAutoFile(dbPath, relPath string) {
	if _, err := os.Stat(dbPath); err != nil {
		return // no index yet — nothing to track
	}
	s.sessionWG.Add(1)
	go func() {
		defer s.sessionWG.Done()
		ctx := context.Background()
		st, err := s.openStore(dbPath)
		if err != nil {
			return
		}
		_ = st.SessionTrackFile(ctx, relPath, "read")
	}()
}

// waitSessionWrites blocks until all in-flight sessionAutoFile goroutines
// finish their store writes. Tests defer it before TempDir cleanup so
// background writes don't race the dir removal.
func (s *Server) waitSessionWrites() { s.sessionWG.Wait() }

// searchThrottleHint increments the repetition counter for (query, project)
// and returns a hint string when the pattern crosses a threshold. Returns ""
// on first few calls. Counters reset after 5 minutes of idle.
func (s *Server) searchThrottleHint(query, project string) string {
	const idleReset = 5 * time.Minute
	key := project + "\x00" + query
	now := time.Now()

	raw, _ := s.searchThrottle.LoadOrStore(key, &throttleEntry{})
	e, ok := raw.(*throttleEntry)
	if !ok {
		return ""
	}

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
		return fmt.Sprintf("find called %d times with identical query — consider storing findings via knowledge action=add instead of re-searching.", count)
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

func (s *Server) FindRelated(ctx context.Context, in FindRelatedInput) (FindRelatedOutput, error) {
	_, out, err := s.findRelated(ctx, nil, in)
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

func (s *Server) Nav(ctx context.Context) (NavOutput, error) {
	_, out, err := s.nav(ctx, nil, NavInput{})
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

func (s *Server) Agent(ctx context.Context, in AgentInput) (AgentOutput, error) {
	_, out, err := s.agent(ctx, nil, in)
	return out, err
}

func (s *Server) Share(ctx context.Context, in ShareInput) (ShareOutput, error) {
	_, out, err := s.share(ctx, nil, in)
	return out, err
}

func (s *Server) CtxPack(ctx context.Context, in PackInput) (PackOutput, error) {
	_, out, err := s.ctxPack(ctx, nil, in)
	return out, err
}

// ─── tool: search_semantic ────────────────────────────────────────────────

type SearchInput struct {
	Query       string   `json:"query" jsonschema:"natural-language or code query"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip (e.g. ['vendor/', 'internal/legacy/'])"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages (e.g. ['go','typescript']); accepts language names or raw extensions (.rs, .go)"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path (e.g. 'internal/**', '**/test*')"`
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
	// Handle is the opaque expansion handle for this hit's range (#344).
	// Echo it into read(handle=…) instead of constructing a path:line.
	// Empty for pseudo-hits that have no concrete file range (start_line 0).
	Handle string `json:"handle,omitempty"`
}

type SearchOutput struct {
	Status   string      `json:"status"`             // "ok" | "no-index" | "embedding-service-unreachable" | "error"
	Hint     string      `json:"hint,omitempty"`     // human-readable suggestion for the model
	Endpoint string      `json:"endpoint,omitempty"` // when unreachable
	Project  string      `json:"project,omitempty"`  // resolved project root
	Stale    bool        `json:"stale,omitempty"`    // last_indexed older than 24h
	Hits     []SearchHit `json:"hits"`
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
	st, err := s.openStore(p.DBPath)
	if err != nil {
		logger.Warn("mcp watch: store open failed", "root", p.Root, "err", err)
		return
	}
	ig, err := ignore.New(p.Root)
	if err != nil {
		logger.Warn("mcp watch: ignore init failed", "root", p.Root, "err", err)
		return
	}

	ixOpts := index.Options{
		Logger:      logger,
		Concurrency: s.AutoWatch.IndexConcurrency,
	}
	ix := index.New(p, st, s.EmbedClient, ig, ixOpts)

	wOpts := watch.Options{
		Debounce: s.AutoWatch.Debounce,
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
			if s.EmbedClient != nil {
				if _, err := graphrefresh.EmbedNodes(c, st, s.EmbedClient, false); err != nil {
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
	}
}

func (s *Server) search(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	out := SearchOutput{Hits: []SearchHit{}}
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

	k, candidateK := clampSearchK(in, p.Root)

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

	st, err := s.openStore(p.DBPath)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("open index: %v", err)
		return nil, out, nil
	}

	stats, err := st.Stats(ctx)
	if err == nil && !stats.LastIndex.IsZero() && time.Since(stats.LastIndex) > 24*time.Hour {
		out.Stale = true
		out.Hint = fmt.Sprintf("index is %s old — results may be stale; run `dex index %s` to refresh.",
			time.Since(stats.LastIndex).Round(time.Hour), p.Root)
	}

	hits, err := st.SearchFused(ctx, vecs[0], in.Query, candidateK)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("search: %v", err)
		return nil, out, nil
	}

	hits = s.applyMultiScaleFilter(ctx, st, p.DBPath, in.Query, hits)

	// Symbol leg: extract identifier tokens from the query, look them up
	// by exact name, and RRF-fuse with the semantic results. Runs in the
	// same request with no extra embedding round-trip — FindSymbol is a
	// pure SQL index scan.
	idents := extractIdentifiers(in.Query)
	if len(idents) > 0 {
		symPool := candidateK * 3
		if symPool < 15 {
			symPool = 15
		}
		if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
			hits = fuseWithSymbols(hits, symHits, k)
		}
	}

	// Graph-proximity lane: spreading activation from session-recent files and
	// the current semantic hits. Silently skips when no session exists or the
	// graph hasn't been built — never fails the search.
	var sessionTask string
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok {
		sessionTask = ss.Task
	}
	hits = st.FuseSpreadingActivation(ctx, hits, vecs[0], candidateK)

	hits, err = st.RerankFused(ctx, in.Query, hits, candidateK)
	if err != nil {
		out.Status = "error"
		out.Hint = fmt.Sprintf("rerank: %v", err)
		return nil, out, nil
	}
	hits = ecsRerank(hits, extractTaskKWs(sessionTask))
	s.activityRecord(p.Root, 1)

	// Apply language and path_glob filters post-ranking, then trim to k.
	exts := langToExtensions(in.Languages)
	hits = filterHits(hits, exts, in.PathGlob, k)

	// Loop detection: block/reduce/hint before building the response.
	ldLevel, ldHint := s.ld().Check("find", argsKey(in.Query), true)
	if ldLevel == ThrottleBlock {
		return nil, SearchOutput{Status: "loop-blocked", Project: p.Root, Hint: ldHint}, nil
	}

	out.Status = "ok"
	if hint := s.searchThrottleHint(in.Query, p.Root); hint != "" {
		out.Hint = hint
	}
	if out.Hint == "" {
		out.Hint = s.activityNudge(p.Root, sessionTask)
	}
	if ldHint != "" && out.Hint == "" {
		out.Hint = ldHint
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
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:     h.Content,
		})
	}
	if ldLevel == ThrottleReduce && len(out.Hits) > 5 {
		out.Hits = out.Hits[:5]
		out.Hint = ldHint + " [reduced: showing top 5]"
	}

	// Stamp expansion handles on the final hit set (#344) — after truncation
	// so we don't mint handles for hits we drop.
	stampSearchHandles(out.Hits)

	// SLO monitoring: record this tool call and check thresholds.
	tr := s.sloFor(p.Root)
	tr.RecordToolCall()
	if ann := sloAnnotation(tr.Check()); ann != "" {
		if out.Hint == "" {
			out.Hint = ann
		} else {
			out.Hint += " " + ann
		}
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
//
// Like the graph lane, both legs are scored from rank position only — the
// incoming Hit.Score magnitude is discarded — so this stage is fusion-mode
// independent (FusionRRF vs FusionLinear changes only the semantic ORDER, not
// the symbol lane's relative weight).
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

// langToExtensions maps human-readable language names to their file extensions.
// Raw extensions (with or without leading dot) pass through unchanged.
func langToExtensions(langs []string) []string {
	aliasMap := map[string][]string{
		"typescript": {"ts", "tsx"},
		"javascript": {"js", "jsx", "mjs", "cjs"},
		"c++":        {"cpp", "hpp", "cc", "hh"},
		"cpp":        {"cpp", "hpp", "cc", "hh"},
		"cc":         {"cpp", "hpp", "cc", "hh"},
		"ruby":       {"rb"},
		"kotlin":     {"kt", "kts"},
		"yaml":       {"yaml", "yml"},
		"yml":        {"yaml", "yml"},
		"python":     {"py"},
		"java":       {"java"},
		"go":         {"go"},
		"rust":       {"rs"},
		"c":          {"c", "h"},
		"swift":      {"swift"},
		"scala":      {"scala"},
		"shell":      {"sh", "bash"},
		"bash":       {"sh", "bash"},
		"html":       {"html", "htm"},
		"css":        {"css"},
		"json":       {"json"},
		"markdown":   {"md", "mdx"},
		"proto":      {"proto"},
		"sql":        {"sql"},
		"toml":       {"toml"},
	}
	seen := make(map[string]struct{})
	var out []string
	for _, lang := range langs {
		normalized := strings.ToLower(strings.TrimSpace(lang))
		if exts, ok := aliasMap[normalized]; ok {
			for _, e := range exts {
				if _, dup := seen[e]; !dup {
					seen[e] = struct{}{}
					out = append(out, e)
				}
			}
		} else {
			// Raw extension: strip leading dot
			ext := strings.TrimPrefix(normalized, ".")
			if _, dup := seen[ext]; !dup {
				seen[ext] = struct{}{}
				out = append(out, ext)
			}
		}
	}
	return out
}

// matchGlob matches pattern against path with support for ** (matches any
// number of path segments). A trailing /** suffix is treated as a directory
// prefix match. Single * is handled by filepath.Match per-segment.
func matchGlob(pattern, path string) bool {
	// Fast path: no double-star — delegate to filepath.Match directly.
	if !strings.Contains(pattern, "**") {
		ok, err := filepath.Match(pattern, path)
		return err == nil && ok
	}
	// Split on ** and match each segment in order.
	parts := strings.Split(pattern, "**")
	remaining := path
	for i, part := range parts {
		if part == "" {
			continue
		}
		// Trim leading separator from part for cleaner matching.
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		if i == 0 {
			// First segment must be a prefix.
			if !strings.HasPrefix(remaining, part) {
				return false
			}
			remaining = remaining[len(part):]
			remaining = strings.TrimPrefix(remaining, "/")
		} else if i == len(parts)-1 {
			// Last segment must be a suffix.
			ok, err := filepath.Match(part, filepath.Base(remaining))
			if err != nil || !ok {
				// Also try matching the full remaining path against part.
				ok2, _ := filepath.Match(part, remaining)
				if !ok2 {
					return false
				}
			}
		} else {
			idx := strings.Index(remaining, part)
			if idx < 0 {
				return false
			}
			remaining = remaining[idx+len(part):]
			remaining = strings.TrimPrefix(remaining, "/")
		}
	}
	return true
}

// filterHits applies optional language (by extension) and path_glob filters,
// then trims to at most limit results. When exts and glob are both empty
// the slice is trimmed to limit unchanged.
// clampSearchK returns the effective k and candidateK from the search input.
// candidateK is inflated when language or path filters are active so post-filter
// trimming still returns k results.
func clampSearchK(in SearchInput, projectRoot string) (k, candidateK int) {
	k = in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	if prof := profiles.Active(projectRoot); prof.Budget.MaxFiles > 0 && k > prof.Budget.MaxFiles {
		k = prof.Budget.MaxFiles
	}
	candidateK = k
	if len(in.Languages) > 0 || in.PathGlob != "" {
		candidateK = k * 10
		if candidateK < 50 {
			candidateK = 50
		}
		if candidateK > 500 {
			candidateK = 500
		}
	}
	return k, candidateK
}

// applyMultiScaleFilter restricts hits to the structurally-relevant files for
// NL and Architecture queries using the in-RAM TF-IDF index. Symbol queries
// and multi-scale build failures are passed through unchanged.
func (s *Server) applyMultiScaleFilter(ctx context.Context, st *store.Store, dbPath, query string, hits []store.Hit) []store.Hit {
	qt := store.ClassifyQueryType(query)
	if qt == store.QueryTypeSymbol {
		return hits
	}
	idx, idxErr := s.cachedBuildMultiScale(ctx, st, dbPath)
	if idxErr != nil || idx == nil {
		return hits
	}
	queryToks := store.TokeniseQuery(query)
	var candidatePaths []string
	switch qt {
	case store.QueryTypeArchitecture:
		dirs := idx.SearchMacro(queryToks, 3)
		candidatePaths = idx.ExpandToFiles(dirs)
		if len(candidatePaths) < 5 {
			candidatePaths = append(candidatePaths, idx.SearchMeso(queryToks, 10)...)
		}
	case store.QueryTypeNL:
		candidatePaths = idx.SearchMeso(queryToks, 8)
	}
	if len(candidatePaths) >= 3 {
		return store.FilterByPaths(hits, candidatePaths)
	}
	return hits
}

func filterHits(hits []store.Hit, exts []string, glob string, limit int) []store.Hit {
	if len(exts) == 0 && glob == "" {
		if len(hits) > limit {
			return hits[:limit]
		}
		return hits
	}
	out := hits[:0]
	for _, h := range hits {
		if len(exts) > 0 {
			ext := strings.TrimPrefix(filepath.Ext(h.Path), ".")
			matched := false
			for _, e := range exts {
				if ext == e {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if glob != "" {
			if !matchGlob(glob, h.Path) {
				continue
			}
		}
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out
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
	Hits    []SearchHit `json:"hits"`
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
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	ldLevel, ldHint := s.ld().Check("lookup", argsKey(in.Name), true)
	if ldLevel == ThrottleBlock {
		return nil, FindSymbolOutput{Status: "loop-blocked", Project: p.Root, Hint: ldHint}, nil
	}

	hits, err := st.FindSymbol(ctx, in.Name, in.K)
	if err != nil {
		return nil, FindSymbolOutput{Status: "error", Hint: fmt.Sprintf("lookup: %v", err)}, nil
	}
	out := FindSymbolOutput{Status: "ok", Project: p.Root, Hits: []SearchHit{}}
	if ldHint != "" {
		out.Hint = ldHint
	}
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
			Role:      formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:   h.Content,
		})
	}
	if ldLevel == ThrottleReduce && len(out.Hits) > 5 {
		out.Hits = out.Hits[:5]
		out.Hint = ldHint + " [reduced: showing top 5]"
	}
	stampSearchHandles(out.Hits)
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
	Hits    []SearchHit `json:"hits"`
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
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	hits, err := st.RelatedChunks(ctx, in.Path, in.StartLine, k)
	if err != nil {
		if strings.Contains(err.Error(), "no chunk at") {
			return nil, RelatedOutput{Status: "not-found", Project: p.Root,
				Hint: err.Error() + " — check that path and start_line match an indexed chunk exactly."}, nil
		}
		return nil, RelatedOutput{Status: "error", Hint: fmt.Sprintf("related: %v", err)}, nil
	}
	out := RelatedOutput{Status: "ok", Project: p.Root, Hits: []SearchHit{}}
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
	stampSearchHandles(out.Hits)
	return nil, out, nil
}

// ─── tool: search_similar ────────────────────────────────────────────────

type FindRelatedInput struct {
	FilePath    string   `json:"file_path" jsonschema:"relative path of the anchor file (e.g. 'internal/mcp/server.go')"`
	Line        int      `json:"line" jsonschema:"line number inside the anchor file (1-indexed); resolves to the containing chunk"`
	ProjectRoot string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	K           int      `json:"k,omitempty" jsonschema:"number of results to return (default 8, max 30)"`
	Exclude     []string `json:"exclude,omitempty" jsonschema:"path prefixes to skip"`
	Languages   []string `json:"languages,omitempty" jsonschema:"restrict results to these languages (e.g. ['go','typescript'])"`
	PathGlob    string   `json:"path_glob,omitempty" jsonschema:"glob pattern matched against relative file path (e.g. 'internal/**')"`
}

type FindRelatedOutput struct {
	Status string `json:"status"` // "ok" | "no-index" | "not-found" | "embedding-service-unreachable" | "error"
	Hint   string `json:"hint,omitempty"`
	// Source is the resolved anchor chunk.
	Source  *SearchHit  `json:"source,omitempty"`
	Project string      `json:"project,omitempty"`
	Hits    []SearchHit `json:"hits"`
}

func (s *Server) findRelated(ctx context.Context, _ *sdk.CallToolRequest, in FindRelatedInput) (*sdk.CallToolResult, FindRelatedOutput, error) {
	if strings.TrimSpace(in.FilePath) == "" {
		return nil, FindRelatedOutput{Status: "error", Hint: "file_path is empty"}, nil
	}
	if in.Line <= 0 {
		return nil, FindRelatedOutput{Status: "error", Hint: "line must be ≥ 1"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, FindRelatedOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, FindRelatedOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}
	candidateK := k
	if len(in.Languages) > 0 || in.PathGlob != "" {
		candidateK = k * 10
		if candidateK < 50 {
			candidateK = 50
		}
		if candidateK > 500 {
			candidateK = 500
		}
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	src, err := st.ChunkAt(ctx, in.FilePath, in.Line)
	if err != nil {
		if strings.Contains(err.Error(), "no chunk at") {
			return nil, FindRelatedOutput{Status: "not-found", Project: p.Root,
				Hint: err.Error() + " — check that file_path and line match an indexed chunk."}, nil
		}
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("chunk lookup: %v", err)}, nil
	}

	em := s.EmbedClient
	vecs, err := em.Embed(ctx, []string{src.Content})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			return nil, FindRelatedOutput{Status: "embedding-service-unreachable",
				Hint: "the local embedding service is offline — fall back to grep / Glob for this query."}, nil
		}
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("embed: %v", err)}, nil
	}

	hits, err := st.SearchFused(ctx, vecs[0], src.Content, candidateK)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// Exclude the source chunk itself.
	filtered := hits[:0]
	for _, h := range hits {
		if h.Path == src.Path && h.StartLine == src.StartLine {
			continue
		}
		filtered = append(filtered, h)
	}
	hits = filtered

	// Symbol leg.
	idents := extractIdentifiers(src.Content)
	if len(idents) > 0 {
		symPool := candidateK * 3
		if symPool < 15 {
			symPool = 15
		}
		if symHits := collectSymbolHits(ctx, st, idents, symPool); len(symHits) > 0 {
			hits = fuseWithSymbols(hits, symHits, candidateK)
			// Re-exclude source after symbol fusion.
			out2 := hits[:0]
			for _, h := range hits {
				if h.Path == src.Path && h.StartLine == src.StartLine {
					continue
				}
				out2 = append(out2, h)
			}
			hits = out2
		}
	}

	// Graph-proximity lane: spreading activation from session-recent files and
	// the current semantic hits. Silently skips when no session exists or the
	// graph hasn't been built — never fails the search.
	hits = st.FuseSpreadingActivation(ctx, hits, vecs[0], candidateK)

	hits, err = st.RerankFused(ctx, src.Content, hits, candidateK)
	if err != nil {
		return nil, FindRelatedOutput{Status: "error", Hint: fmt.Sprintf("rerank: %v", err)}, nil
	}

	var sessionTask string
	if ss, ok, err2 := st.SessionGet(ctx); err2 == nil && ok {
		sessionTask = ss.Task
	}
	hits = ecsRerank(hits, extractTaskKWs(sessionTask))

	exts := langToExtensions(in.Languages)
	hits = filterHits(hits, exts, in.PathGlob, k)

	out := FindRelatedOutput{
		Status:  "ok",
		Project: p.Root,
		Hits:    []SearchHit{},
		Source: &SearchHit{
			Path:      src.Path,
			Kind:      src.Kind,
			StartLine: src.StartLine,
			EndLine:   src.EndLine,
			Content:   src.Content,
		},
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
			Role:        formatRole(h.Name, h.InDegree, h.OutDegree, h.CrossPkgCallers, h.Betweenness),
			Content:     h.Content,
		})
	}
	stampSearchHandles(out.Hits)
	return nil, out, nil
}

// ─── tool: view_summarize ─────────────────────────────────────────────────

type SummarizeInput struct {
	Path         string   `json:"path,omitempty" jsonschema:"file path to summarize; relative paths are resolved against project_root; required when paths is not set"`
	Paths        []string `json:"paths,omitempty" jsonschema:"batch mode: list of files (max 10); all use the same mode; path is ignored when paths is non-empty"`
	ProjectRoot  string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
	Mode         string   `json:"mode,omitempty" jsonschema:"read fidelity: 'full' (default, summarize via LLM), 'skeleton' (exported signatures + @B<n> body handles, no LLM), 'signatures' (indexed symbols + source lines, no LLM), 'map' (imports + exported symbols from index, no LLM), 'lines:N-M' (raw line slice, no LLM)"`
	StartLine    int      `json:"start_line,omitempty" jsonschema:"first line to summarize (1-indexed, inclusive); 0 = beginning of file"`
	EndLine      int      `json:"end_line,omitempty" jsonschema:"last line to summarize (1-indexed, inclusive); 0 = end of file"`
	Focus        string   `json:"focus,omitempty" jsonschema:"optional steering — e.g. 'public API surface', 'side effects', 'error handling'"`
	Temperature  float32  `json:"temperature,omitempty" jsonschema:"sampling temperature (0 = server default)"`
	MaxTokens    int      `json:"max_tokens,omitempty" jsonschema:"maximum tokens to generate (0 = server default)"`
	Etag         string   `json:"etag,omitempty" jsonschema:"content hash from a prior read; if the file is unchanged the server returns status=unchanged — re-use the content already in context instead of re-reading"`
	BudgetTokens int      `json:"budget_tokens,omitempty" jsonschema:"optional remaining context budget in tokens; when set, dex auto-downgrades mode to fit (full→skeleton→signatures→map→handle) — omit for no budget constraint"`
	Task         string   `json:"task,omitempty" jsonschema:"optional current task description (e.g. from the session tool); when set, dex selects the compression level automatically — Generate/Test tasks use aggressive (no LLM), others use lightweight cleanup"`
	// CacheLayout overrides the profile's cache_layout knob for this call.
	// Values: "stable_first" (default), "recency", "off". Empty means use profile default.
	CacheLayout string `json:"cache_layout,omitempty" jsonschema:"batch ordering policy for prompt-cache hits: stable_first (session-seen files first), recency (caller order), off"`
	// Expand retrieves a suppressed function/method body from a previous skeleton-mode
	// read. Pass the handle key from the skeleton output (e.g. "@B3").
	Expand string `json:"expand,omitempty" jsonschema:"expand a body handle issued by a previous skeleton-mode read, e.g. '@B3'; returns the full source lines for that scope"`
	// Handle is an expansion handle (#344) minted by find/ask/lookup. When set it
	// decodes to a concrete path + line range and supersedes path/paths/start_line/
	// end_line — the agent echoes the opaque token instead of constructing a
	// reference, so it can never read a path it invented. Distinct from `expand`,
	// which addresses suppressed bodies within one skeleton read.
	Handle string `json:"handle,omitempty" jsonschema:"expansion handle from a find/ask/lookup result (the result's 'handle' field); reads that exact range — supersedes path/paths/start_line/end_line"`
}

type SummarizeOutput struct {
	Status       string   `json:"status"` // "ok" | "unchanged" | "delta" | "chat-service-unreachable" | "bad-handle" | "error"
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
	// StablePrefixTokens is the estimated token count of the stable-prefix
	// section when cache_layout=stable_first reordering was applied to a batch
	// call. Zero for single-file calls or when no stable files were found.
	// Place the Anthropic cache_control breakpoint after this many tokens from
	// the start of the response to maximise prompt-cache hits.
	StablePrefixTokens int `json:"stable_prefix_tokens,omitempty"`
}

// maxSummarizeBytes caps the slice we send to the chat endpoint. Above
// this the local model's quality drops sharply and latency spikes;
// callers wanting a whole-repo overview should use ask_codebase with
// RAG instead. Tuned to fit comfortably in a 32B-coder context window
// alongside the system prompt and the summary itself.
const maxSummarizeBytes = 64 * 1024

func (s *Server) summarize(ctx context.Context, req *sdk.CallToolRequest, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) { //nolint:cyclop
	out := SummarizeOutput{}

	// Expansion handle (#344): decode it to a concrete path + line range that
	// supersedes any path/paths/lines the caller also passed. A token that fails
	// to decode is rejected here (status="bad-handle") so a hallucinated handle
	// never reaches the filesystem. Existence of the decoded path is enforced
	// downstream by the normal path resolution + stat below.
	if h := strings.TrimSpace(in.Handle); h != "" {
		path, start, end, ok := DecodeHandle(h)
		if !ok {
			return nil, SummarizeOutput{Status: "bad-handle", Hint: "handle did not decode to a valid path:line range; re-run find/ask to get a fresh handle"}, nil
		}
		in.Path = path
		in.StartLine = start
		in.EndLine = end
		in.Paths = nil
		// #355 F2: a handle encodes an exact range to read. Pin a lines: mode so
		// the profile/task resolution below can't downgrade to a whole-file
		// compressed view (signatures/map/aggressive/skeleton) that silently
		// drops the range — only the `full` and `lines:` branches honor the
		// decoded StartLine/EndLine. A lines: pin is chat-independent (unlike
		// `full`, which the lean profile downgrades back to `map`, re-dropping
		// the range) and returns exactly the requested slice. An explicit caller
		// mode still wins: the handle supersedes path/lines, not a deliberate
		// mode choice.
		if strings.TrimSpace(in.Mode) == "" {
			in.Mode = fmt.Sprintf("lines:%d-%d", start, end)
		}
	}

	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		// Apply profile default_mode when no explicit mode was passed.
		if in.ProjectRoot != "" {
			if prof := profiles.Active(in.ProjectRoot); prof.Read.DefaultMode != "" {
				mode = prof.Read.DefaultMode
			}
		}
		if mode == "" {
			mode = "full"
		}
	}
	isFull := mode == "full"

	// Task-aware mode selection (#130): when the caller declares a task and
	// hasn't forced a specific mode, override to the most appropriate compression.
	// Generate/Test → aggressive (no LLM, comments stripped); others stay as-is.
	if in.Task != "" && mode == "full" {
		if override := compress.TaskToMode(in.Task); override != "" {
			// Adaptive policy (#109): if this (intent, mode) pair has been penalized
			// by prior output-ratio feedback, downgrade to a less lossy mode.
			if p2, h2 := s.resolveProject(in.ProjectRoot); h2 == "" {
				pt := compress.LoadPolicy(p2.CacheDir)
				override = pt.ChooseMode(compress.IntentFromTask(in.Task), override)
			}
			mode = override
			isFull = false
		}
	}

	if isFull && s.ChatClient == nil {
		mode = "map"
		isFull = false
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

	// SLO monitoring: record the tool call, check for throttle/block.
	{
		tr := s.sloFor(p.Root)
		tr.RecordToolCall()
		if tr.ConsumeThrottle() && mode == "full" {
			// Throttle: downgrade full→signatures to reduce token output.
			mode = "signatures"
			isFull = false
		}
		if blockMsg := sloBlock(tr.Check()); blockMsg != "" {
			return nil, SummarizeOutput{Status: "error", Hint: blockMsg}, nil
		}
	}

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
	// Heatmap recording (#108): on every successful file_view, record the
	// access and compression savings. Fires after the function returns so
	// out.Bytes and out.Status are final. Best-effort — never blocks the read.
	cacheDir := p.CacheDir
	sloTracker := s.sloFor(p.Root)
	defer func() {
		if out.Status != "ok" {
			return
		}
		hm := heatmap.Load(cacheDir)
		origTok := out.Bytes / 4
		compTok := len(out.Content) / 4
		saved := origTok - compTok
		if saved < 0 {
			saved = 0
		}
		hm.RecordAccess(relTarget, origTok, saved)
		_ = hm.Save(cacheDir)

		// SLO: record output tokens and append any warn annotations.
		sloTracker.RecordTokens(len(out.Content) / 4)
		if ann := sloAnnotation(sloTracker.Check()); ann != "" {
			if out.Hint == "" {
				out.Hint = ann
			} else {
				out.Hint += " " + ann
			}
		}
	}()
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

	sessionID := "stdio" // fallback: stdio transport returns "" from ID()
	if req != nil && req.Session != nil {
		if id := req.Session.ID(); id != "" {
			sessionID = id
		}
	}
	if in.Etag != "" && in.Etag == etag && s.readCacheCheck(sessionID, relTarget, etag) {
		return nil, SummarizeOutput{Status: "unchanged", Project: out.Project, Path: relTarget, Etag: etag}, nil
	}

	// Delta re-read (#217): file changed since last delivery — try a compact unified diff.
	// Skip when the caller is expanding a body handle (handled below) or mode=full with
	// a live chat client (LLM summary; diff of raw bytes != diff of two summaries).
	if in.Expand == "" && !(isFull && s.ChatClient != nil) {
		if prevData, ok := s.readCacheGetContent(sessionID, relTarget); ok {
			if delta, worth := computeLineDelta(prevData, data); worth {
				out.Status = "delta"
				out.Etag = etag
				out.Bytes = len(data)
				out.Content = delta
				s.readCacheMark(sessionID, relTarget, etag)
				s.readCacheSetContent(sessionID, relTarget, data)
				return nil, out, nil
			}
		}
	}
	// Store raw bytes so future changed re-reads can produce a delta.
	defer func() {
		if out.Status == "ok" {
			s.readCacheSetContent(sessionID, relTarget, data)
		}
	}()

	// Body handle expansion (#206): @B<n> handle from a prior skeleton-mode read.
	if in.Expand != "" {
		h, ok := s.lookupBodyHandle(sessionID, in.Expand)
		if !ok {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("unknown handle %q — issue a skeleton-mode read first", in.Expand)}, nil
		}
		if h.etag != etag {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file has changed since handle %q was issued — re-read with mode=skeleton", in.Expand)}, nil
		}
		slice, sliceStart, sliceEnd := sliceLines(data, h.startLine, h.endLine)
		out.Status = "ok"
		out.Etag = etag
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		out.Bytes = len(slice)
		out.Content = string(slice)
		out.Hint = fmt.Sprintf("body expansion of %s (lines %d-%d)", in.Expand, sliceStart, sliceEnd)
		s.readCacheMark(sessionID, relTarget, etag)
		return nil, out, nil
	}

	// Bounce detection (#98): if this file was recently delivered compressed
	// and the agent is re-requesting it, escalate to full mode.
	bt := s.bt()
	bt.recordRead(sessionID, relTarget)
	if bt.shouldForceFull(sessionID, relTarget) && mode != "full" {
		mode = "full"
		isFull = mode == "full" && s.ChatClient != nil
	}

	// Budget-aware downgrade (#106): auto-select the richest mode that fits
	// within the caller's remaining context budget. No-op when BudgetTokens=0.
	if in.BudgetTokens > 0 && !isFull {
		fileTokens := len(data) / 4 // ~4 bytes per token (rough approximation)
		mode = selectAffordableMode(mode, fileTokens, in.BudgetTokens)
	}

	// Dependency manifest shortcut (#125): for package.json, go.mod, Cargo.toml,
	// etc. return a compact summary directly — 10-50× token reduction.
	if compress.IsDepsFilename(filepath.Base(realTarget)) && mode != "full" {
		if summary, ok := compress.CompressDepsFile(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Bytes = len(data)
			out.Content = summary
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
	}

	switch {
	case strings.HasPrefix(mode, "lines:"):
		rest := strings.TrimPrefix(mode, "lines:")
		start, end, ok := parseLinesRange(rest)
		if !ok {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("invalid lines mode %q — expected lines:N-M (e.g. lines:10-40)", in.Mode)}, nil
		}
		slice, sliceStart, sliceEnd := sliceLines(data, start, end)
		if sliceStart > sliceEnd {
			fileLines := chunk.LineCount(data)
			return nil, SummarizeOutput{
				Status: "error",
				Hint:   fmt.Sprintf("line range %d-%d is past EOF (file has %d lines)", start, end, fileLines),
			}, nil
		}
		out.StartLine = sliceStart
		out.EndLine = sliceEnd
		out.Bytes = len(slice)
		out.Status = "ok"
		out.Etag = etag
		out.Content = string(slice)
		s.readCacheMark(sessionID, relTarget, etag)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "signatures":
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		if len(syms) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		content := formatSignatures(data, syms, relTarget, nil)
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
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "map":
		// N14: non-code files get a pure-Go structural outline; no index needed.
		if content, ok := compress.NonCodeMap(relTarget, data); ok {
			out.Status = "ok"
			out.Etag = etag
			out.Content = content
			out.Bytes = len(content)
			s.readCacheMark(sessionID, relTarget, etag)
			return nil, out, nil
		}
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
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
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "aggressive":
		ext := filepath.Ext(realTarget)
		// Weak target_model profiles get the anchor-verbatim floor (#291).
		strict := profiles.Active(p.Root).StrictAnchors()
		content := compress.CompressCode(string(data), ext, strict)
		// Semantic chunk reordering (#105): when a task is provided, reorder
		// compressed content so the most task-relevant blocks appear first.
		if in.Task != "" {
			content = applySemanticChunkOrder(content, in.Task)
		}
		out.Status = "ok"
		out.Etag = etag
		out.Content = content
		out.Bytes = len(content)
		origLines := bytes.Count(data, []byte("\n")) + 1
		compLines := strings.Count(content, "\n") + 1
		if origLines > compLines {
			out.Hint = fmt.Sprintf("aggressive: %d → %d lines (%.0f%% reduction)",
				origLines, compLines, float64(origLines-compLines)*100/float64(origLines))
		}
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
		return nil, out, nil

	case mode == "skeleton":
		// Skeleton mode (#206): exported type declarations in full; exported
		// function/method bodies replaced with @B<n> handles; unexported omitted.
		// Falls back to signatures when the index has no symbols.
		st, err := s.openStore(p.DBPath)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
		}
		syms, err := st.SymbolsByFile(ctx, relTarget)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("symbol query: %v", err)}, nil
		}
		if len(syms) == 0 {
			out.Status = "ok"
			out.Hint = "no indexed symbols for this file — run `dex index` first or use mode=full"
			return nil, out, nil
		}
		scopes := make([]compress.BodyScope, 0, len(syms))
		for _, sym := range syms {
			exported := len(sym.Name) > 0 && sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
			scopes = append(scopes, compress.BodyScope{
				Name:      sym.QualifiedName,
				Kind:      sym.Kind,
				Exported:  exported,
				StartLine: sym.StartLine,
				EndLine:   sym.EndLine,
			})
		}
		res := compress.SkeletonPass(data, relTarget, scopes)
		s.registerBodyHandles(sessionID, relTarget, etag, res.Bodies)
		out.Status = "ok"
		out.Etag = etag
		out.Content = res.Text
		out.Bytes = len(res.Text)
		s.readCacheMark(sessionID, relTarget, etag)
		bt.recordCompressed(sessionID, relTarget)
		s.sessionAutoFile(p.DBPath, relTarget)
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
			out.Hint = fmt.Sprintf("⚠ Large file (%d lines): pass mode=skeleton, mode=signatures, or mode=map to reduce tokens.", lineCount)
		}

		system := buildSummarizeSystem(in.Focus)
		cleaned := compress.LightweightCleanup(string(slice))
		userContent := fmt.Sprintf("FILE: %s (lines %d-%d)\n\n```\n%s\n```",
			relTarget, sliceStart, sliceEnd, cleaned)

		chatMsgs := []chat.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: userContent},
		}
		chatOpts := chat.Options{Temperature: in.Temperature, MaxTokens: in.MaxTokens}
		var resp chat.Response
		if req != nil && req.Session != nil {
			sess := req.Session
			resp, err = s.ChatClient.GenerateStream(ctx, chatMsgs, chatOpts, func(tok string) {
				_ = sess.Log(ctx, &sdk.LoggingMessageParams{
					Level:  "debug",
					Logger: "dex/file_view",
					Data:   tok,
				})
			})
		} else {
			resp, err = s.ChatClient.Generate(ctx, chatMsgs, chatOpts)
		}
		if err != nil {
			hint := fmt.Sprintf("chat error (%v) — showing raw content", err)
			if errors.Is(err, chat.ErrUnreachable) {
				hint = "chat service offline — showing raw content"
			}
			out.Status = "ok"
			out.Etag = etag
			out.Content = string(slice)
			out.Hint = hint
			s.readCacheMark(sessionID, relTarget, etag)
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
		s.activityRecord(p.Root, 1)
		return nil, out, nil
	}
}

// summarizeBatch handles file_view when paths[] is provided.
// All files are processed with the same mode in a single call.
// When 3+ files are successfully read, a TF-IDF codebook is applied to
// replace repeated lines (imports, boilerplate) with short §N refs.
func (s *Server) summarizeBatch(ctx context.Context, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	const maxBatch = 10
	if len(in.Paths) > maxBatch {
		return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("batch too large: max %d files per call, got %d", maxBatch, len(in.Paths))}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "signatures"
	}

	// Stable-first layout: load session-stable file set before the per-file
	// loop so we can annotate results as they come in.
	stableSet := batchStableSet(ctx, in.ProjectRoot, s.IndexDir)

	type fileResult struct {
		path    string // resolved path (or "" for errors)
		header  string // "## <path>" or "## <rawPath>\n⚠ <hint>"
		content string // file content (empty for errors)
		ok      bool
		stable  bool // session-seen before this turn
	}

	results := make([]fileResult, 0, len(in.Paths))
	var project string

	for _, rawPath := range in.Paths {
		single := in
		single.Path = rawPath
		single.Paths = nil
		single.Mode = mode
		_, out, err := s.summarize(ctx, nil, single)
		if err != nil {
			return nil, SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s: %v", rawPath, err)}, nil
		}
		if project == "" {
			project = out.Project
		}
		if out.Status != "ok" {
			results = append(results, fileResult{
				header: fmt.Sprintf("## %s\n⚠ %s", rawPath, out.Hint),
			})
			continue
		}
		results = append(results, fileResult{
			path:    out.Path,
			header:  fmt.Sprintf("## %s", out.Path),
			content: out.Content,
			ok:      true,
			stable:  stableSet[out.Path],
		})
	}

	// cache_layout=stable_first (default): move session-stable files to the
	// front so the Anthropic prompt cache can build a consistent prefix across
	// turns. Preserve relative order within each tier. No-op when no session
	// exists, only one file, or the profile opts out.
	// Per-call override wins; fall back to profile; fall back to stable_first.
	layout := in.CacheLayout
	if layout == "" {
		layout = profiles.Active(in.ProjectRoot).Read.CacheLayout
	}
	if layout == "" {
		layout = "stable_first" // default
	}
	if layout == "stable_first" && len(results) > 1 {
		stable := results[:0:0]
		fresh := results[:0:0]
		for _, r := range results {
			if r.ok && r.stable {
				stable = append(stable, r)
			} else {
				fresh = append(fresh, r)
			}
		}
		results = append(stable, fresh...)
	}

	// Build codebook from successfully-read file contents.
	var fileContents []string
	for _, r := range results {
		if r.ok {
			fileContents = append(fileContents, r.content)
		}
	}
	cb := compress.BuildCodebook(fileContents)

	var sb strings.Builder
	if !cb.Empty() {
		sb.WriteString(cb.Legend())
		sb.WriteByte('\n')
	}

	var resolvedPaths []string
	var stablePrefixTokens int
	countingStable := layout == "stable_first"
	for _, r := range results {
		if r.ok {
			content := cb.Apply(r.content)
			chunk := fmt.Sprintf("%s\n%s\n\n", r.header, content)
			sb.WriteString(chunk)
			resolvedPaths = append(resolvedPaths, r.path)
			if countingStable && r.stable {
				stablePrefixTokens += compress.EstimateTokens(chunk)
			} else {
				countingStable = false // stop counting once we hit a fresh file
			}
		} else {
			fmt.Fprintf(&sb, "%s\n\n", r.header)
			if countingStable {
				countingStable = false
			}
		}
	}

	return nil, SummarizeOutput{
		Status:             "ok",
		Project:            project,
		Content:            strings.TrimRight(sb.String(), "\n"),
		Paths:              resolvedPaths,
		StablePrefixTokens: stablePrefixTokens,
	}, nil
}

// batchStableSet returns the set of project-relative file paths that appear
// in the current session — these are "session-stable" for cache layout
// purposes. Returns an empty (non-nil) map when no session exists or on any
// error (failures are silently swallowed; this is a best-effort optimisation).
func batchStableSet(ctx context.Context, projectRoot, indexDir string) map[string]bool {
	root := projectRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return map[string]bool{}
		}
	}
	p, err := proj.Resolve(root, indexDir)
	if err != nil {
		return map[string]bool{}
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		return map[string]bool{}
	}
	ss, ok, err := st.SessionGet(ctx)
	if err != nil || !ok {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(ss.Files))
	for _, f := range ss.Files {
		set[f.Path] = true
	}
	return set
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

// formatSignatures produces a compact symbol index for a file.
// Each exported symbol gets its declaration line; unexported symbols are
// listed without source. Output is ~10× smaller than mode=full.
func formatSignatures(src []byte, syms []store.GraphSymbol, relPath string, _ []string) string {
	srcLines := bytes.Split(bytes.TrimRight(src, "\n"), []byte("\n"))
	totalLines := bytes.Count(src, []byte("\n")) + 1
	var b strings.Builder
	fmt.Fprintf(&b, "%s %dL (%d symbols)\n\n", relPath, totalLines, len(syms))

	isTypeKind := func(kind string) bool {
		return kind == "struct" || kind == "interface" || kind == "type"
	}
	// Only top-level named symbols (func/type/var/const) count as exported,
	// not struct fields, imports, or file-level nodes.
	exported := func(sym store.GraphSymbol) bool {
		if sym.Kind == "field" || sym.Kind == "import" || sym.Kind == "file" {
			return false
		}
		return len(sym.Name) > 0 && sym.Name[0] >= 'A' && sym.Name[0] <= 'Z'
	}
	writeSym := func(sym store.GraphSymbol) {
		si := sym.StartLine - 1
		exp := exported(sym)
		if exp {
			marker := "⊛"
			fmt.Fprintf(&b, "%s %s (lines %d-%d)\n", marker, sym.QualifiedName, sym.StartLine, sym.EndLine)
			if si >= 0 && si < len(srcLines) {
				b.Write(srcLines[si])
				b.WriteByte('\n')
			}
		} else {
			fmt.Fprintf(&b, "  %s %s (lines %d-%d)\n", sym.Kind, sym.QualifiedName, sym.StartLine, sym.EndLine)
		}
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
	ID          string `json:"id"`
	Root        string `json:"root,omitempty"`
	Chunks      int    `json:"chunks"`
	Files       int    `json:"files"`
	Dim         int    `json:"dim"`
	EmbedModel  string `json:"embed_model,omitempty"`
	LastIndexed string `json:"last_indexed,omitempty"`
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
		Version:  Version,
		IndexDir: s.IndexDir,
	}
	if s.EmbedClient != nil {
		out.Endpoint = s.EmbedClient.Endpoint()
		out.Model = s.EmbedClient.ModelName()
	} else {
		// Lean profile (DEX_EMBED_ENGINE=none): no embedder wired.
		out.Model = "none (lean profile)"
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
	if s.EmbedClient != nil {
		probe(&wg, s.EmbedClient, func(ok bool, errMsg string) {
			out.Reachable = ok
			out.Error = errMsg
		})
	}
	if s.ChatClient != nil {
		probe(&wg, s.ChatClient, func(ok bool, _ string) { out.ChatReachable = ok })
	}
	if s.RerankClient != nil {
		probe(&wg, s.RerankClient, func(ok bool, _ string) { out.RerankReachable = ok })
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
				st, err := s.openStore(path)
				if err != nil {
					return
				}
				stats, _ := st.Stats(ctx)
				root, _ := st.ProjectRoot(ctx)
				st.Close()
				ps := ProjectStatus{
					ID:         id,
					Root:       root,
					Chunks:     stats.Chunks,
					Files:      stats.Files,
					Dim:        stats.Dim,
					EmbedModel: stats.EmbedModel,
				}
				if !stats.LastIndex.IsZero() {
					ps.LastIndexed = stats.LastIndex.Format(time.RFC3339)
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
	defer s.waitSessionWrites() // flush pending session records before returning

	srv := sdk.NewServer(&sdk.Implementation{
		Name:    "dex",
		Version: Version,
	}, &sdk.ServerOptions{
		Instructions: ServerInstructions(),
	})

	registerTools(srv, s, s.ChatClient != nil, s.EmbedClient != nil, profiles.Active("").StrictAnchors(), descriptionModeFromEnv())

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
	findRelated(context.Context, *sdk.CallToolRequest, FindRelatedInput) (*sdk.CallToolResult, FindRelatedOutput, error)
	graphDeps(context.Context, *sdk.CallToolRequest, GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error)
	graphCallers(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphCallees(context.Context, *sdk.CallToolRequest, CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error)
	graphImpact(context.Context, *sdk.CallToolRequest, ImpactInput) (*sdk.CallToolResult, ImpactOutput, error)
	graphLinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphBacklinks(context.Context, *sdk.CallToolRequest, DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error)
	graphTags(context.Context, *sdk.CallToolRequest, TagInput) (*sdk.CallToolResult, TagOutput, error)
	graphCycles(context.Context, *sdk.CallToolRequest, CyclesInput) (*sdk.CallToolResult, CyclesOutput, error)
	graphPath(context.Context, *sdk.CallToolRequest, PathInput) (*sdk.CallToolResult, PathOutput, error)
	graphDiff(context.Context, *sdk.CallToolRequest, DiffInput) (*sdk.CallToolResult, DiffOutput, error)
	graphCommunities(context.Context, *sdk.CallToolRequest, CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error)
	overview(context.Context, *sdk.CallToolRequest, OverviewInput) (*sdk.CallToolResult, OverviewOutput, error)
	smells(context.Context, *sdk.CallToolRequest, SmellsInput) (*sdk.CallToolResult, SmellsOutput, error)
	routes(context.Context, *sdk.CallToolRequest, RoutesInput) (*sdk.CallToolResult, RoutesOutput, error)
	searchTree(context.Context, *sdk.CallToolRequest, SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error)
	searchGrep(context.Context, *sdk.CallToolRequest, SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error)
	knowledge(context.Context, *sdk.CallToolRequest, KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error)
	session(context.Context, *sdk.CallToolRequest, SessionInput) (*sdk.CallToolResult, SessionOutput, error)
	compressOutput(context.Context, *sdk.CallToolRequest, CompressInput) (*sdk.CallToolResult, CompressOutput, error)
	shellRun(context.Context, *sdk.CallToolRequest, ShellInput) (*sdk.CallToolResult, ShellOutput, error)
	status(context.Context, *sdk.CallToolRequest, StatusInput) (*sdk.CallToolResult, StatusOutput, error)
	summarize(context.Context, *sdk.CallToolRequest, SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error)
	compose(context.Context, *sdk.CallToolRequest, ComposeInput) (*sdk.CallToolResult, ComposeOutput, error)
	specVerify(context.Context, *sdk.CallToolRequest, SpecVerifyInput) (*sdk.CallToolResult, SpecVerifyOutput, error)
	agent(context.Context, *sdk.CallToolRequest, AgentInput) (*sdk.CallToolResult, AgentOutput, error)
	share(context.Context, *sdk.CallToolRequest, ShareInput) (*sdk.CallToolResult, ShareOutput, error)
	ctxPack(context.Context, *sdk.CallToolRequest, PackInput) (*sdk.CallToolResult, PackOutput, error)
	nav(context.Context, *sdk.CallToolRequest, NavInput) (*sdk.CallToolResult, NavOutput, error)
	feedback(context.Context, *sdk.CallToolRequest, FeedbackInput) (*sdk.CallToolResult, FeedbackOutput, error)
	prefetch(context.Context, *sdk.CallToolRequest, PrefetchInput) (*sdk.CallToolResult, PrefetchOutput, error)
	workspaceSearch(context.Context, *sdk.CallToolRequest, WorkspaceSearchInput) (*sdk.CallToolResult, WorkspaceSearchOutput, error)
}

// addTool registers h on srv with a panic recovery guard. A handler panic is
// converted to a structured tool error instead of crashing the MCP session.
// (The go-sdk v1.6.1 dispatch loop has no recover() of its own.)
func addTool[In, Out any](srv *sdk.Server, t *sdk.Tool, h sdk.ToolHandlerFor[In, Out]) {
	sdk.AddTool(srv, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (res *sdk.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("mcp handler panic", "tool", req.Params.Name, "panic", r, "stack", string(debug.Stack()))
				err = fmt.Errorf("internal error: handler panic: %v", r)
				res = &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("internal error: %v", r)}}}
			}
		}()
		return h(ctx, req, in)
	})
}

// registerTools wires the dex tool surface onto srv, dispatching to h.
// Exposure is capability-derived (#283/#290): the embedder-backed lanes
// (semantic search behind `find`/`ask`) are only registered when
// embedAvailable is true. With no embedder wired (the lean profile,
// DEX_EMBED_ENGINE=none), those are omitted entirely and the surface degrades
// to BM25 (`grep`) + symbol + graph + file lanes; `ask` stays and routes to
// the non-semantic lanes. chatAvailable gates `read` the same way. When
// weakModel is true the full tool surface is hidden and only ask, grep, ls,
// and shell are exposed.
func registerTools(srv *sdk.Server, h toolSurface, chatAvailable, embedAvailable, weakModel bool, descMode DescriptionMode) {
	td := func(s string) string { return compressToolDesc(s, descMode) }
	expert := expertEnabled()
	if !weakModel {
		// Default verb surface (#316 story 3): map (orient) / find (search) /
		// trace (call graph) / impact (blast radius) / read (digest); ask is
		// always-on below. The granular lanes these verbs compose over move
		// behind DEX_EXPERT to keep the everyday surface small.
		addTool(srv, &sdk.Tool{
			Name:        "map",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Orient in an unfamiliar codebase: a deterministic, multi-zoom map of the " +
				"project's top packages/dirs and how they connect — no embedding or chat required. " +
				"Call this FIRST when you don't yet know where things live, before find/ask fan-out. " +
				"Returns 'no-index' when the project hasn't been indexed yet."),
		}, mapHandler(h))

		if embedAvailable {
			addTool(srv, &sdk.Tool{
				Name:        "find",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Prefer `ask` for general code-understanding questions — it composes this " +
					"tool with symbol lookup and graph expansion. Use `find` directly only when you specifically " +
					"want raw ranking without intent routing. " +
					"Embeds the query and returns top-k matching chunks. Identifier tokens in the query (CamelCase, " +
					"snake_case, qualified names) are automatically looked up by exact symbol name and fused into the " +
					"results via Reciprocal Rank Fusion — no separate `lookup` call needed. " +
					"Supports exclude list to skip paths. " +
					"Optional 'languages' (e.g. ['go','typescript']) and 'path_glob' (e.g. 'internal/**') narrow results " +
					"to specific file types or directories; when active, candidates are over-fetched to compensate for filtering. " +
					"On error, returns a structured status: 'no-index' (run dex index first), " +
					"'embedding-service-unreachable' (fall back to grep), or 'ok'."),
			}, h.search)
		}

		addTool(srv, &sdk.Tool{
			Name:        "trace",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Walk the static call graph from a symbol. `direction`: 'callers' (default — " +
				"who calls it), 'callees' (what it calls), or 'path' (shortest call route to the `to` symbol). " +
				"Go-only for now; other languages fall back to ripgrep via `ask`. Accepts a bare name ('Foo'), " +
				"receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer'). " +
				"Returns 'no-graph' when calls edges haven't been indexed (`dex index . --graph=only`)."),
		}, traceHandler(h))

		if expert {
			addTool(srv, &sdk.Tool{
				Name:        "lookup",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Prefer `ask` — it detects identifiers in your question and runs this " +
					"lookup automatically as part of a fused response. Use `lookup` directly only when you " +
					"already have the exact identifier name and want nothing else. " +
					"Fast SQL lookup — no embedding required. Returns 'not-found' when no chunk with that name exists."),
			}, h.findSymbol)

			addTool(srv, &sdk.Tool{
				Name:        "deps",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Return the `imports` edges for a file or package — the package the file belongs to, " +
					"and the list of packages it depends on. Sourced from the static graph (no embedding, no chat). " +
					"Pass `path` (relative file inside the project) OR `package` (full package path). " +
					"Returns 'no-index' / 'no-graph' / 'not-found' when the project, graph, or symbol is missing."),
			}, h.graphDeps)

			addTool(srv, &sdk.Tool{
				Name:        "callers",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Return functions that CALL the given symbol, from the static graph's `calls` edges. " +
					"Go-only for now (Python/JS/Rust callers fall back to ripgrep via `ask`). " +
					"Accepts a bare name (`Foo`), a qualified method (`(*Server).RunStdio`), or a package-qualified " +
					"name (`mcp.NewServer`). Multiple matches are returned with their package paths so the agent can " +
					"disambiguate. Returns 'no-graph' when calls edges haven't been indexed yet."),
			}, h.graphCallers)

			addTool(srv, &sdk.Tool{
				Name:        "callees",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Return functions that the given symbol CALLS, from the static graph's `calls` edges. " +
					"Go-only for now. Same name resolution as `callers`. " +
					"Returns 'no-graph' when calls edges haven't been indexed yet."),
			}, h.graphCallees)
		}

		addTool(srv, &sdk.Tool{
			Name:        "impact",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Transitive blast-radius analysis. Given a symbol, follows `calls` edges " +
				"in the callers direction up to max_depth (default 3) and returns every reachable function " +
				"with its hop depth and PageRank. Depth 1 = direct callers; depth 2 = their callers; etc. " +
				"Use before editing a widely-called symbol to gauge the ripple. " +
				"Same name resolution as `trace`. Returns 'no-graph' when calls edges haven't been indexed yet."),
		}, h.graphImpact)

		if expert {
			addTool(srv, &sdk.Tool{
				Name:        "routes",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Detect HTTP handlers, MCP tool registrations, and gRPC service implementations " +
					"from the call graph. Matches ServeHTTP implementations, handle*/serve*-named functions, " +
					"and callers of registration functions (Handle, HandleFunc, AddTool, RegisterService, etc.). " +
					"Returns each handler with its file location and the registration function that wires it in. " +
					"Requires a graph index (`dex index . --graph=only`)."),
			}, h.routes)

			addTool(srv, &sdk.Tool{
				Name:        "smells",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("AST-based code quality signals derived from the graph index — no LLM required. " +
					"Returns four categories: `long_functions` (bodies >= min_func_lines, default 80), " +
					"`dead_exports` (exported functions/methods with no indexed callers), " +
					"`god_files` (files with >= min_file_symbols symbols, default 30), and " +
					"`god_nodes` (functions/methods with in_degree >= min_god_node_callers (20) OR " +
					"cross_pkg_callers >= min_god_node_pkg_callers (8) — over-coupled symbols constraining many callers). " +
					"Requires a graph index (`dex index . --graph=only`). Use before a PR or refactor to spot obvious structural issues."),
			}, h.smells)

			addTool(srv, &sdk.Tool{
				Name:        "path",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Find the shortest call/import path between two symbols in the graph. " +
					"BFS over `calls` and `imports` edges from src to dst. " +
					"Returns an ordered list of hops (symbol + edge_kind leading into it). " +
					"Status `no-path` means no route within max_depth (default 8). " +
					"Requires a graph index (`dex index . --graph=only`)."),
			}, h.graphPath)

			addTool(srv, &sdk.Tool{
				Name:        "diff",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Blast-radius analysis for a git diff. " +
					"Runs `git diff --name-only <ref> HEAD` to find changed files, " +
					"collects all function/method nodes in those files as seeds, " +
					"then BFS over `calls` edges to find transitive callers (default depth 2, max 5). " +
					"Returns the blast-radius node list sorted by depth and PageRank. " +
					"Requires a graph index (`dex index . --graph=only`)."),
			}, h.graphDiff)

			addTool(srv, &sdk.Tool{
				Name:        "clusters",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("List Louvain communities in the call/import graph — " +
					"clusters of tightly-interconnected symbols. " +
					"Communities are sorted by descending size. " +
					"Top members per community are sorted by PageRank. " +
					"Community IDs are stable across re-runs for unchanged subgraphs. " +
					"Requires a graph index (`dex index . --graph=only`). " +
					"Useful for understanding module boundaries, finding hidden coupling, and planning refactors."),
			}, h.graphCommunities)

			addTool(srv, &sdk.Tool{
				Name:        "status",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Report dex endpoint health and the list of indexed projects with their chunk counts and last-indexed times."),
			}, h.status)

			addTool(srv, &sdk.Tool{
				Name: "notes",
				Description: td("Manage persistent project knowledge — facts, patterns, and gotchas that survive " +
					"session resets and reconnects. Actions: add (store a fact with an archetype and confidence), " +
					"list (retrieve top-k facts ordered by salience), delete (remove a fact by id). " +
					"Archetypes: Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact. " +
					"High-salience facts (Architecture, Gotcha) are automatically injected into ask responses " +
					"as knowledge_facts. No embedding required."),
			}, h.knowledge)

			addTool(srv, &sdk.Tool{
				Name: "session",
				Description: td("Manage per-project session memory across tool calls. " +
					"Actions: set_task (declare what you're working on), add_note (record a finding or decision), " +
					"add_file (track a file you read/wrote), get (retrieve the current session state), " +
					"clear (reset the session), snapshot (generate a recovery block after context compaction), " +
					"budget (estimate context window utilization — returns used_tokens, remaining_tokens, utilization 0–1, and a recommendation: normal/compress/evict/critical), " +
					"heatmap (show per-file access frequency and compression savings — hot/cold file breakdown, useful for spotting orphaned or rarely-read files). " +
					"Session state (task + notes + files) is surfaced in ask responses as session_task so you " +
					"don't lose context across reconnects. No embedding required."),
			}, h.session)
		}

		if chatAvailable {
			addTool(srv, &sdk.Tool{
				Name:        "read",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Prefer `ask` first — its `suggested_reads` will name the file worth " +
					"summarizing. Use `read` directly only when you already know which file you need digested. " +
					"Sends the file slice directly to the chat model. Pass `focus` to steer (e.g. 'public API surface'). " +
					"Path must resolve inside project_root. Files larger than 64 KB are truncated. " +
					"Pass paths[] (up to 10) to read multiple files in one call — all use the same mode. " +
					"Re-read savings: every response includes `etag` (content hash). On re-reads pass that etag back; " +
					"if the file is unchanged the server returns status=unchanged — reuse the content already in context. " +
					"If the file changed since the last read the server may return status=delta with a compact unified diff " +
					"in Content (saves 40-60% tokens vs re-sending the full file); update your mental model from the diff. " +
					"Pass `task` (your current task from `session`) to get automatic compression routing: " +
					"Generate/Test tasks use aggressive mode (strips comments, no LLM call), others apply lightweight cleanup. " +
					"Skeleton mode (mode=skeleton): emits exported type declarations in full and function/method signatures " +
					"with @B<n> body handles. Expand a body on demand: pass expand='@B<n>' on a subsequent call. " +
					"On error, returns 'chat-service-unreachable' or 'error'."),
			}, h.summarize)
		}
	}

	addTool(srv, &sdk.Tool{
		Name: "shell",
		Description: td("Execute a shell command and return compressed output. " +
			"Applies the same compression pipeline as compress_output — collapses build noise, " +
			"deduplicates log lines, strips ANSI, and summarises go test / git / cargo / npm / docker output — " +
			"so raw command output never hits your context budget. " +
			"Use raw:true to skip compression. " +
			"File-write redirects (> >>) and tee are blocked; use the Write tool instead. " +
			"Timeout: 60 s."),
	}, h.shellRun)

	addTool(srv, &sdk.Tool{
		Name:        "ls",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("List indexed files under a directory path. Returns individual files within " +
			"`depth` directory levels (default 3) and aggregates deeper files into their parent dirs " +
			"(dirs shown with trailing / and a summed chunk count). " +
			"No embedding required — reads directly from the index. " +
			"Use for orientation in an unfamiliar codebase before calling ask or read. " +
			"Returns 'no-index' when the project hasn't been indexed yet."),
	}, h.searchTree)

	addTool(srv, &sdk.Tool{
		Name:        "grep",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Regex search over project files — no embedding required. " +
			"Complements ask/find for exact-match queries: cross-cutting symbol references, " +
			"import paths, string literals, or patterns that semantic search misses. " +
			"Searches the indexed file list when available (respects .gitignore via the index); " +
			"falls back to walking the project directory and skipping .git/vendor/node_modules. " +
			"Accepts an RE2 regex pattern, optional relative path prefix, and optional extension filter. " +
			"Returns up to max_results matches (default 50) with path, line number, and trimmed content. " +
			"Returns 'no-matches' when nothing matches. Use ask for conceptual queries."),
	}, h.searchGrep)

	addTool(srv, &sdk.Tool{
		Name:        "ask",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("PRIMARY ENTRY POINT for code-understanding questions — and, by default, the ONLY dex tool you " +
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
			"only to override. Returns 'no-index' / 'embedding-service-unreachable' for graceful fallback to grep."),
	}, h.contextRouter)

}

// Version is set at build time via -ldflags.
var Version = "dev"
