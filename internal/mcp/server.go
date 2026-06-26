// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graphrefresh"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/slo"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
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
- find(query, path_glob)   instead of Grep/rg for concept/intent searches
- trace(symbol, direction) instead of manual cross-ref tracing (callers/callees/path)
- impact(name)             instead of guessing an edit's blast radius
- read(path)               instead of Read for large files (signatures + summaries)
- shell(command)           instead of Bash for shell commands (compressed output)
- grep(pattern)            instead of rg for exact regex matches

Workflow:
1. Orient: ask(question) — routes intent, returns suggested_reads + next_action; map() for layout
2. Locate: find for concepts; trace to follow the call graph
3. Read: read for large files; native Read for small ones
4. Shell: shell(command) for build/test output

Power lanes (lookup, deps, diff, clusters, routes, smells, status, notes, session) are gated behind DEX_EXPERT — the verbs above cover everyday work.

Start every session by calling ask() with the task description.

IMPORTANT: dex MCP tools are deferred — their schemas are not loaded until you call ToolSearch. Before using any dex tool for the first time each session, call ToolSearch with query="select:mcp__dex__ask,mcp__dex__shell,mcp__dex__ls,mcp__dex__find,mcp__dex__grep,mcp__dex__read" to load the schemas. Do this before any other action.`
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
	// MaxDelay caps how long a never-quiet stream of saves can defer a
	// re-index (default 5s; negative disables). See watch.Options.MaxDelay.
	MaxDelay time.Duration
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
	// after 7 the search is skipped and the hint signals to use cached findings.
	// Counters reset after 10 minutes of idle.
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
	answerCache retrieve.AnswerCache
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
	loop     *throttle.Detector
	loopOnce sync.Once
}

type Server struct {
	EmbedClient  embed.Embedder
	ChatClient   chat.Chatter         // optional — when nil, view_summarize is not registered
	RerankClient rerank.HealthChecker // optional — only consulted by `status` for health reporting; the actual rerank wiring goes through Retrieve / StoreOpts.Rerank
	ExpandClient chat.Chatter         // optional — drives opt-in query-side expansion (#252); nil disables it
	ExpandMode   string               // server default expand level (off|on|full) when a request omits it
	IndexDir     string               // base dir holding per-project index folders
	StoreOpts    store.Options        // applied to every Store opened by the server
	Retrieve     retrieve.Service     // query-time ranking service; holds the cross-encoder + shared rerank cache (#473)
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

func (s *Server) ld() *throttle.Detector {
	s.loopOnce.Do(func() { s.loop = throttle.New() })
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
// and returns a hint string when the pattern crosses a threshold. The second
// return value is true at 7+ repetitions — callers should skip re-running the
// expensive search and return just the hint. Counters reset after 10 minutes
// of idle on that pattern.
func (s *Server) searchThrottleHint(query, project string) (hint string, earlyReturn bool) {
	const idleReset = 10 * time.Minute
	key := project + "\x00" + query
	now := time.Now()

	raw, _ := s.searchThrottle.LoadOrStore(key, &throttleEntry{})
	e, ok := raw.(*throttleEntry)
	if !ok {
		return "", false
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
		return fmt.Sprintf("[dex: %d repeated searches for this pattern — returning cached result from first search. Use notes action=add to record what you found.]", count), true
	case count >= 4:
		return fmt.Sprintf("[dex: this question has been asked %d times this session — consider recording findings with notes action=add]", count), false
	}
	return "", false
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

func (s *Server) Knowledge(ctx context.Context, in KnowledgeInput) (KnowledgeOutput, error) {
	_, out, err := s.knowledge(ctx, nil, in)
	return out, err
}

func (s *Server) Session(ctx context.Context, in SessionInput) (SessionOutput, error) {
	_, out, err := s.session(ctx, nil, in)
	return out, err
}

func (s *Server) Budget(ctx context.Context, in BudgetInput) (BudgetOutput, error) {
	_, out, err := s.budget(ctx, nil, in)
	return out, err
}

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
	locate(context.Context, *sdk.CallToolRequest, LocateInput) (*sdk.CallToolResult, LocateOutput, error)
	review(context.Context, *sdk.CallToolRequest, ReviewInput) (*sdk.CallToolResult, ReviewOutput, error)
	refactor(context.Context, *sdk.CallToolRequest, RefactorInput) (*sdk.CallToolResult, RefactorOutput, error)
	cohort(context.Context, *sdk.CallToolRequest, CohortInput) (*sdk.CallToolResult, CohortOutput, error)
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
	graphCycles(context.Context, *sdk.CallToolRequest, CyclesInput) (*sdk.CallToolResult, CyclesOutput, error)
	graphPath(context.Context, *sdk.CallToolRequest, PathInput) (*sdk.CallToolResult, PathOutput, error)
	graphDiff(context.Context, *sdk.CallToolRequest, DiffInput) (*sdk.CallToolResult, DiffOutput, error)
	graphCommunities(context.Context, *sdk.CallToolRequest, CommunitiesInput) (*sdk.CallToolResult, CommunitiesOutput, error)
	smells(context.Context, *sdk.CallToolRequest, SmellsInput) (*sdk.CallToolResult, SmellsOutput, error)
	routes(context.Context, *sdk.CallToolRequest, RoutesInput) (*sdk.CallToolResult, RoutesOutput, error)
	searchTree(context.Context, *sdk.CallToolRequest, SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error)
	searchGrep(context.Context, *sdk.CallToolRequest, SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error)
	knowledge(context.Context, *sdk.CallToolRequest, KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error)
	session(context.Context, *sdk.CallToolRequest, SessionInput) (*sdk.CallToolResult, SessionOutput, error)
	checkpoint(context.Context, *sdk.CallToolRequest, CheckpointInput) (*sdk.CallToolResult, CheckpointOutput, error)
	shellRun(context.Context, *sdk.CallToolRequest, ShellInput) (*sdk.CallToolResult, ShellOutput, error)
	status(context.Context, *sdk.CallToolRequest, StatusInput) (*sdk.CallToolResult, StatusOutput, error)
	summarize(context.Context, *sdk.CallToolRequest, SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error)
	budget(context.Context, *sdk.CallToolRequest, BudgetInput) (*sdk.CallToolResult, BudgetOutput, error)
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
// the non-semantic lanes. `read` is always registered (its structural modes
// need no chat); only `read mode=summary` needs a chat model and returns
// status='needs-chat' when chatAvailable is false. When weakModel is true the
// full tool surface is hidden and only ask, grep, ls, and shell are exposed.
func registerTools(srv *sdk.Server, h toolSurface, chatAvailable, embedAvailable, weakModel bool, descMode DescriptionMode) {
	_ = chatAvailable // read no longer gates on chat; summary degrades at call time
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
				"Go edges are type-resolved; Python/JS/TS/Rust/Java are name-based (tree-sitter) with incomplete " +
				"recall, so an empty result there is not proof of none — verify with grep. Accepts a bare name ('Foo'), " +
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
			// callers/callees are not standalone tools — `trace --dir
			// callers|callees` is the single call-graph entry point (#575).
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

		// locate is in the default lane (#636 / GitHub #65 S1): one-call
		// orientation around a code location. It composes the everyday lanes
		// (resolve → callers → tests → nearest doc → churn → notes) that an
		// agent otherwise stitches together by hand dozens of times a session.
		addTool(srv, &sdk.Tool{
			Name:        "locate",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("One-call orientation around a code location. Give any one of `ref` " +
				"('path:line'), `symbol` (a name), or `frame` (a raw stack-trace line) and locate resolves " +
				"the enclosing symbol, then returns its callers (+ risk tier), sibling test files, the nearest " +
				"doc (CLAUDE.md/doc.go/README.md walking up), the file's last commit + author, and related " +
				"project notes — in one response. Pass `issues: true` to also list matching open GitHub issues " +
				"via `gh` (best-effort). Pure composition over the index; needs no chat model. Degrades cleanly: " +
				"callers are empty when the graph isn't indexed; returns 'no-index' / 'not-found' otherwise. " +
				"Use this BEFORE fanning out trace/find/read to orient on a path:line, symbol, or panic frame."),
		}, h.locate)

		// review is in the default lane (#639 / GitHub #65 S2): per-hunk PR
		// intelligence. Code review is delta-shaped while every other verb is
		// state-shaped — review composes the diff with callers, tests, churn,
		// author history, and notes per hunk so the agent spends its budget on
		// judgment, not context assembly.
		addTool(srv, &sdk.Tool{
			Name:        "review",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Per-hunk intelligence for a diff or PR — use this when reviewing changes. " +
				"Give one of `ref` ('HEAD~3..HEAD' or a single ref vs HEAD), `branch` (what it adds since " +
				"diverging from the default branch), or `pr` (a GitHub PR number, resolved via `gh`). " +
				"For each changed hunk review returns the touched symbols, their callers (+ a risk tier from " +
				"caller blast radius and export status), and related notes; per file it adds sibling tests, the " +
				"nearest doc, 30-day churn, last commit/author, and recent author history. " +
				"Pass `compact: true` to drop low-risk hunks. Pure composition over the index; needs no chat " +
				"model. Degrades cleanly: callers/risk are empty when the graph isn't indexed (diff + churn " +
				"still returned); returns 'no-index' / 'no-changes' / 'not-found' otherwise."),
		}, h.review)

		// refactor is in the default lane (#638 / GitHub #65 S3): type-precise
		// edit planning. dex stays read-only (#551) — refactor never writes; it
		// returns byte-precise edit triples the agent applies with Edit. v1 is
		// rename_symbol for Go, planned on-demand via go/packages (no index).
		addTool(srv, &sdk.Tool{
			Name:        "refactor",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Plan a type-precise rename and get back byte-exact edit triples to apply yourself " +
				"(dex never writes files). Set `op` to 'rename_symbol' (default), `symbol` to the target " +
				"(bare 'Foo', receiver-qualified '(*Server).Run', or pkg-qualified 'mcp.NewServer') and `to` to " +
				"the new name. Returns every (path, start_byte, end_byte, replacement) edit across the module, " +
				"resolved by the Go type checker — a method rename touches only that type's method, never " +
				"same-named methods elsewhere. Apply edits highest-offset-first per file. The `etag` echoes the " +
				"touched files' hash; pass it back to detect a stale plan. Go-only in v1 (returns " +
				"'unsupported-language' otherwise); also 'not-found' / 'ambiguous' / 'stale'. Loads packages " +
				"on-demand, so it is slower than the read verbs — reach for it when you're about to rename."),
		}, h.refactor)

		// notes is in the default lane (#548): persistent project memory is the
		// highest-leverage saver of repeat exploration, and the read path (facts
		// auto-injected into ask) is useless if the agent can never write. Needs
		// no embedder or chat model.
		addTool(srv, &sdk.Tool{
			Name: "notes",
			Description: td("Persistent project memory — record and recall facts, patterns, and gotchas that " +
				"survive session resets and reconnects (no embedding required). " +
				"Actions: add (store a fact with an archetype and confidence — the response's " +
				"`similar` list warns when a near-duplicate note already exists so you can `delete` " +
				"the superseded one), list (recall top-k facts ordered by salience), delete (remove a fact by id). " +
				"Archetypes: Architecture | Gotcha | Convention | Decision | Observation | Dependency | Pattern | Fact. " +
				"High-salience facts (Architecture, Gotcha) are automatically injected into ask responses " +
				"as knowledge_facts."),
		}, h.knowledge)

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

			// cohort (#643): blast radius of an intent. Given an interface, list
			// the types you must edit in lockstep when its method set changes —
			// complete implementors plus near-misses (the backend you forgot).
			addTool(srv, &sdk.Tool{
				Name:        "cohort",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Find the types that must change together with an interface. Given an `interface` " +
					"name (bare 'toolSurface' or pkg-qualified 'mcp.toolSurface'), returns every type that " +
					"implements it ('complete') plus near-misses that implement most of it but are missing methods " +
					"('partial' — the backend you forgot to update), each with its declaration file:line and the " +
					"missing method names. Pure go/types — no index needed; Go-only (returns 'unsupported-language' " +
					"otherwise). Reach for it before adding/removing an interface method to plan the lockstep edit."),
			}, h.cohort)
			// `path` is not a standalone tool — `trace --dir path --to <dst>`
			// finds the shortest route between two symbols (#575).

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
				Name:        "budget",
				Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
				Description: td("Per-session context budget radar (#33). Retrospective view of tokens actually " +
					"emitted by dex's tools this session: context_tokens, tool_calls, shell_calls, plus the " +
					"top files by net token footprint (original − compressed-savings) from the heatmap. " +
					"Surfaces any active SLO violations and a one-line hint when a file dominates. " +
					"Complements `session action=budget`, which is a prospective estimate from declared session state."),
			}, h.budget)

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

			addTool(srv, &sdk.Tool{
				Name: "checkpoint",
				Description: td("Private shadow git history of the working tree — checkpoint and review your " +
					"own work-in-progress WITHOUT touching the user's .git (a separate repo under dex's cache). " +
					"Actions: snapshot (commit the current working tree to the shadow; idempotent — no commit when " +
					"unchanged; returns sha + files_changed), log (list checkpoints, newest first; limit default 20/max 200), " +
					"diff (unified diff between two checkpoints, default HEAD~1..HEAD; from/to override; byte-capped). " +
					"Use it to review what you've changed across a session, or to snapshot before a risky refactor. " +
					"Read-only on the user's tree (dex never writes it, #551): apply any rollback yourself from the diff."),
			}, h.checkpoint)
		}

		addTool(srv, &sdk.Tool{
			Name:        "read",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Prefer `ask` first — its `suggested_reads` will name the file worth reading. " +
				"Use `read` directly when you already know which file you need. " +
				"`mode` (default 'full') selects the view: 'full' = raw file content (no LLM, exact bytes); " +
				"'signatures' = indexed symbols + source lines; 'skeleton' = exported type declarations in full plus " +
				"function/method signatures with @B<n> body handles (expand one later via expand='@B<n>'); " +
				"'map' = imports + exported symbols; 'lines:N-M' = raw line slice; " +
				"'analyze' = a token-cost comparison of every mode plus a recommended mode and NO file content, so you " +
				"can pick the cheapest sufficient view before paying to read it; " +
				"'summary' = LLM-generated digest (the only mode needing a chat model — pass `focus` to steer, " +
				"e.g. 'public API surface'; returns status='needs-chat' when no chat model is wired). " +
				"Path must resolve inside project_root. Files larger than 64 KB are truncated. " +
				"Pass paths[] (up to 10) to read multiple files in one call — all use the same mode. " +
				"Re-read savings: every response includes `etag` (content hash). On re-reads pass that etag back; " +
				"if the file is unchanged the server returns status=unchanged — reuse the content already in context. " +
				"If the file changed since the last read the server may return status=delta with a compact unified diff " +
				"in Content (saves 40-60% tokens vs re-sending the full file); update your mental model from the diff. " +
				"Pass `task` (your current task from `session`) for automatic compression routing of the raw default. " +
				"On error, returns 'chat-service-unreachable' or 'error'."),
		}, h.summarize)
	}

	addTool(srv, &sdk.Tool{
		Name: "shell",
		Description: td("Execute a shell command and return compressed output. " +
			"Applies the same compression pipeline as compress_output — collapses build noise, " +
			"deduplicates log lines, strips ANSI, and summarises go test / git / cargo / npm / docker output — " +
			"so raw command output never hits your context budget. " +
			"Use raw:true to skip compression. " +
			"Runs via bash when available (falls back to POSIX sh), so pipefail and bash-only " +
			"syntax work; override with DEX_SHELL. " +
			"File-write redirects (> >>) and tee are blocked by default; use the Write tool instead, " +
			"or set DEX_SHELL_ALLOW_WRITES=1 to permit them. " +
			"On a non-zero exit whose output matches a known failure signature, the response carries a " +
			"low-confidence `gotcha_candidate` — confirm it with `notes` (action=add) to persist the pitfall. " +
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
			"callers/callees intents (Go is type-resolved; other languages are name-based via tree-sitter, with a ripgrep usage list as backup). Inline content " +
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

// Version is the build version. A release build overrides it via
// -ldflags "-X .../internal/mcp.Version=<v>" (mooncake task install). When it
// is still "dev" — e.g. a plain `go install` — resolveVersion (version.go)
// recovers the VCS revision Go embeds in the build info.
var Version = "dev"
