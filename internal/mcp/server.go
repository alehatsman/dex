// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
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
	"github.com/alehatsman/dex/internal/veccache"
	"github.com/alehatsman/dex/internal/watch"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerInstructions returns the MCP server instructions block that Claude Code
// receives at session init. It maps dex tools to their native equivalents so
// the agent uses them without being explicitly asked.
func ServerInstructions() string {
	return `dex is active — prefer its MCP tools over native equivalents:

dex is advisory-only — exactly two effects: query (read the intelligence) · record (write a durable finding). Running commands, editing, and verifying are the harness's job.
1. query(input) — START HERE for any coding task or question. Its input SHAPE picks the lane, and the answer's precision tracks the input's: a file path → its compressed signatures (raw bytes: use native Read), a path:line or range → that slice, a /regex/ → grep, a bare symbol ('NewServer', '(*Server).Run') → JUST its call graph, a prose question ('how are edits debounced?') → a ranked semantic evidence pack. Force the lane with kind=… and the facet with want=… (want=assemble for a task-start working set; kind=review for a working-tree review). The envelope's route echoes the shape detected and the road not taken.
2. edit / run / verify — your job (native Edit + Bash), not dex's.

Tool mapping (use these instead of native):
- query(input)    instead of Grep/rg/Read/manual navigation — one read verb: a path → signatures, a /regex/ → grep, a path:line → slice, a symbol → call graph, prose → routed evidence pack (semantic + symbol + graph). Raw file bytes are still the native Read tool's job.
- record(fact)  instead of re-deriving facts — write a durable finding, recall with query=…, or correct a stale one with fact + supersedes=<id>

Power lanes (gated behind DEX_EXPERT — the two verbs above cover everyday work):
- grep / read — the raw primitives query wraps; reach here for the primitive directly
- review_diff — targeted PR/branch/ref review (query kind=review covers the working tree)
- trace / locate / search / deps / clusters / routes / smells / clones / similar / cohort / refs / status / repo_map — call-graph, structural, and vector lanes: search returns raw ranked hits with the full scoring breakdown, trace walks callers/callees/path/impact, clones/similar are vector work grep can't do

IMPORTANT: dex MCP tools are deferred — call ToolSearch with query="select:mcp__dex__query,mcp__dex__record" before first use.`
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
	EmbedClient    embed.Embedder
	ChatClient     chat.Chatter         // optional — when nil, view_summarize is not registered
	ChatConfigured bool                 // true when a chat model was actually wired (explicit DEX_CHAT_MODEL or detected ollama model); gates the status chat probe so an unconfigured default isn't reported as DEGRADED (#133)
	RerankClient   rerank.HealthChecker // optional — only consulted by `status` for health reporting; the actual rerank wiring goes through Retrieve / StoreOpts.Rerank
	ExpandClient   chat.Chatter         // optional — drives opt-in query-side expansion (#252); nil disables it
	ExpandMode     string               // server default expand level (off|on|full) when a request omits it
	IndexDir       string               // base dir holding per-project index folders
	StoreOpts      store.Options        // applied to every Store opened by the server
	Retrieve       retrieve.Service     // query-time ranking service; holds the cross-encoder + shared rerank cache (#473)
	AutoWatch      AutoWatchConfig      // lazy per-project watcher; zero value disables
	CCRDir         string               // optional override for the proxy CCR tee dir; defaults to ~/.cache/dex/proxy/tee (#630)

	watcherState  // project watcher goroutines
	sessionState  // per-MCP-session tracking (throttle, dedup, body handles)
	cacheState    // in-memory read/content/answer caches
	patternState  // loop and bounce-thrash detection
	feedbackState // live feedback reader + shadow reweight A/B (#731)
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

// registerBodyHandles assigns session-global sequence numbers to a slice of
// BodyEntry handles and stores them in the per-session map. Returns the
// remapped body entries (with session-global N values) and a strings.Replacer
// that maps file-local @B1/@B2/… to the session-global keys, so callers can
// fix up the skeleton text produced by compress.SkeletonPass.
//
// Without session-global numbering, two successive skeleton reads of different
// files both produce @B1, @B2, … and the second registration overwrites the
// first, causing expand=@B1 to resolve against the wrong file.
func (s *Server) registerBodyHandles(sessionID, relPath, etag string, bodies []compress.BodyEntry) (remapped []compress.BodyEntry, r *strings.Replacer) {
	if sessionID == "" || len(bodies) == 0 {
		return bodies, strings.NewReplacer()
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
	base := s.bodyHandlesSeq[sessionID]
	pairs := make([]string, 0, len(bodies)*2)
	remapped = make([]compress.BodyEntry, len(bodies))
	for i, be := range bodies {
		newN := base + i + 1
		newKey := fmt.Sprintf("@B%d", newN)
		remapped[i] = compress.BodyEntry{N: newN, Name: be.Name, StartLine: be.StartLine, EndLine: be.EndLine}
		s.bodyHandles[sessionID][newKey] = bodyHandle{
			relPath:   relPath,
			startLine: be.StartLine,
			endLine:   be.EndLine,
			etag:      etag,
		}
		// Map file-local key to session-global key.
		pairs = append(pairs, fmt.Sprintf("@B%d", be.N), newKey)
	}
	s.bodyHandlesSeq[sessionID] = base + len(bodies)
	return remapped, strings.NewReplacer(pairs...)
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
// readCacheCheck returns true when sessionID already received relPath (in the
// given mode) at etag — so the caller can short-circuit with "unchanged".
// mode is included in the key so switching modes on the same file never
// returns "unchanged" when new output is expected (#770).
func (s *Server) readCacheCheck(sessionID, relPath, etag, mode string) bool {
	if sessionID == "" {
		return false
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readCache == nil {
		return false
	}
	return s.readCache[sessionID][relPath+"\x00"+mode] == etag
}

// readCacheMark records that sessionID has received relPath (in mode) at etag.
func (s *Server) readCacheMark(sessionID, relPath, etag, mode string) {
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
	s.readCache[sessionID][relPath+"\x00"+mode] = etag
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

func (s *Server) IndexStatus(ctx context.Context, in IndexStatusInput) (IndexStatusOutput, error) {
	_, out, err := s.indexStatus(ctx, nil, in)
	return out, err
}

// resolveProject maps a caller-supplied project_root to a Project. Precedence
// when project_root is empty: the client's declared workspace root (MCP
// roots/list, reached via the session stashed in ctx by addTool) first, then
// the server's own cwd as a last resort. The cwd backstop is no longer silent —
// it warns once per distinct fallback root so a wrong-worktree read is visible.
func (s *Server) resolveProject(ctx context.Context, projectRoot string) (*proj.Project, string) {
	root := projectRoot
	if root == "" {
		if l := listerFromContext(ctx); l != nil {
			root = rootFromClient(ctx, l, s.IndexDir)
		}
	}
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, "could not determine project root; pass project_root explicitly"
		}
		root = wd
		warnCwdFallback(wd)
	}
	p, err := proj.Resolve(root, s.IndexDir)
	if err != nil {
		return nil, fmt.Sprintf("resolve project: %v", err)
	}
	s.markForeground(p)
	s.ensureWatcher(p)
	return p, ""
}

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

// mcpResumeStateEnv carries a JSON-serialized sdk.ServerSessionState across
// a SIGUSR1 exec-reload so the new binary resumes the MCP session without a
// re-initialization handshake (Claude Code sees no reconnect gap).
const mcpResumeStateEnv = "DEX_MCP_RESUME_STATE"

func (s *Server) RunStdio(ctx context.Context) error {
	s.runCtx = ctx
	defer s.watcherWG.Wait()
	defer s.waitSessionWrites() // flush pending session records before returning

	sdkSrv := sdk.NewServer(&sdk.Implementation{
		Name:    "dex",
		Version: Version,
	}, &sdk.ServerOptions{
		Instructions: ServerInstructions(),
	})

	profiles.Active("") // prime default-profile token-family detection at registration time
	registerTools(sdkSrv, s, s.EmbedClient != nil, descriptionModeFromEnv())

	// Restore session state from a prior exec-reload (set by the SIGUSR1
	// handler below). Clear it immediately so child processes don't inherit it.
	var opts *sdk.ServerSessionOptions
	if raw := os.Getenv(mcpResumeStateEnv); raw != "" {
		_ = os.Unsetenv(mcpResumeStateEnv)
		var state sdk.ServerSessionState
		if err := json.Unmarshal([]byte(raw), &state); err == nil && state.InitializeParams != nil {
			opts = &sdk.ServerSessionOptions{State: &state}
		}
	}

	ss, err := sdkSrv.Connect(ctx, &sdk.StdioTransport{}, opts)
	if err != nil {
		return err
	}

	// On SIGUSR1, exec-reload: replace this process with the newly installed
	// binary. The stdin/stdout pipe is inherited so Claude Code sees no
	// disconnection. The new binary picks up the session via mcpResumeStateEnv.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGUSR1)
		defer signal.Stop(ch)
		select {
		case <-ctx.Done():
		case <-ch:
			if params := ss.InitializeParams(); params != nil {
				state := sdk.ServerSessionState{
					InitializeParams:  params,
					InitializedParams: new(sdk.InitializedParams),
				}
				if b, err := json.Marshal(state); err == nil {
					exe, _ := os.Executable()
					env := append(os.Environ(), mcpResumeStateEnv+"="+string(b))
					syscall.Exec(exe, os.Args, env) //nolint:errcheck,gosec
				}
			}
			// Fallback if state is unavailable: exit cleanly so the client reconnects.
			os.Exit(0)
		}
	}()

	ssClosed := make(chan error, 1)
	go func() { ssClosed <- ss.Wait() }()
	select {
	case <-ctx.Done():
		_ = ss.Close()
		<-ssClosed
		return ctx.Err()
	case err := <-ssClosed:
		return err
	}
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
	rehearse(context.Context, *sdk.CallToolRequest, RehearseInput) (*sdk.CallToolResult, RehearseOutput, error)
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
	clones(context.Context, *sdk.CallToolRequest, ClonesInput) (*sdk.CallToolResult, ClonesOutput, error)
	routes(context.Context, *sdk.CallToolRequest, RoutesInput) (*sdk.CallToolResult, RoutesOutput, error)
	searchTree(context.Context, *sdk.CallToolRequest, SearchTreeInput) (*sdk.CallToolResult, SearchTreeOutput, error)
	searchGrep(context.Context, *sdk.CallToolRequest, SearchGrepInput) (*sdk.CallToolResult, SearchGrepOutput, error)
	knowledge(context.Context, *sdk.CallToolRequest, KnowledgeInput) (*sdk.CallToolResult, KnowledgeOutput, error)
	check(context.Context, *sdk.CallToolRequest, CheckInput) (*sdk.CallToolResult, CheckOutput, error)
	refs(context.Context, *sdk.CallToolRequest, RefsInput) (*sdk.CallToolResult, RefsOutput, error)
	status(context.Context, *sdk.CallToolRequest, StatusInput) (*sdk.CallToolResult, StatusOutput, error)
	summarize(context.Context, *sdk.CallToolRequest, SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error)
	indexStatus(context.Context, *sdk.CallToolRequest, IndexStatusInput) (*sdk.CallToolResult, IndexStatusOutput, error)
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
		// Carry the client session so resolveProject can consult the caller's
		// declared workspace roots (#120) without threading req through every
		// handler.
		res, out, err = h(withSession(ctx, req.Session), req, in)
		// Stamp the uniform cost envelope (#110 step 2): tokens_returned on every
		// four-verb success, plus budget_left when the input carried a budget.
		// No-op for tools whose output doesn't implement costStamper.
		if err == nil {
			stampEnvelopeCost(&out, in)
		}
		return res, out, err
	})
}

// registerTools wires the dex tool surface onto srv, dispatching to h.
// Exposure is capability-derived (#283/#290): the embedder-backed lanes
// (semantic search behind `find`/`ask`) are only registered when
// embedAvailable is true. With no embedder wired (the lean profile,
// DEX_EMBED_ENGINE=none), those are omitted entirely and the surface degrades
// to BM25 + symbol + graph + file lanes (reached via `look` and `ask`, which
// routes to the non-semantic lanes). After the 5c collapse (#145) the always-on
// floor is the index-free verbs `ask` + `look` (fetch) + `act` (run); the raw
// primitives they subsume — `shell`, `grep`, `read` — moved to the expert lane.
//
// Named profiles, not a boolean matrix (#110 step 8, spec: tool-surface.md).
// The deployment shape is one of three profiles — full (embedder+chat),
// bm25-only (no embedder), lean (weak local model) — but the VERB SET is
// constant across all three: query · record. A profile changes only query's
// internal capability (synthesis → lexical → hits-only, degraded at call
// time from embed/chat), never which tools an agent sees. record registers even
// for a weak local model: a weaker model forgets more, so durable memory helps it
// most. DEX_EXPERT is an additive power-lane overlay, orthogonal to the profile —
// never a different shape of the everyday surface.
func registerTools(srv *sdk.Server, h toolSurface, embedAvailable bool, descMode DescriptionMode) {
	td := func(s string) string { return compressToolDesc(s, descMode) }

	// The two-verb surface (#197, epic #195): query (read the intelligence) +
	// record (write a durable finding). dex is advisory-only — running commands
	// is the harness's job, so act/shell/verify/checkpoint are gone.
	registerQueryTool(srv, h, td)                     // query — the single read verb (merges ask + look)
	registerEverydayTools(srv, h, td, embedAvailable) // record (durable memory)

	// DEX_EXPERT overlays the granular power lanes additively, in any profile.
	if expertEnabled() {
		registerExpertTools(srv, h, td, embedAvailable)
	}
}

// registerEverydayTools wires the write verb of the two-verb surface: record
// (durable memory). Read moves live in query (path/regex/symbol/prose → the one
// classifier); record absorbed notes' everyday moves (#147: write/recall/
// supersede), leaving notes' admin/relate tail on the CLI (`dex notes`).
func registerEverydayTools(srv *sdk.Server, h toolSurface, td func(string) string, embedAvailable bool) {
	_ = embedAvailable // no everyday tool is embed-gated after the 5c/5d collapse

	// record — the durable-memory verb (#195, renamed from remember to avoid the
	// cognition metaphor). An envelope facade over the knowledge engine covering
	// the memory hot path: `fact` writes, `query` recalls, `supersedes` upserts
	// (#147). Same store and salience as the CLI `dex notes` admin surface, which
	// retains the admin/relate tail. No embedder needed.
	addTool(srv, &sdk.Tool{
		Name: "record",
		Description: td("Durable project memory across session resets, inside the universal envelope " +
			"{result, trust, next}. Three moves: pass `fact` to persist a durable fact (write — lead a " +
			"review finding or gotcha with a bracketed [kind]; use `scope` to bind it to a file glob so it " +
			"surfaces on touch, #645), pass `query` to recall the facts most relevant to a task (read — " +
			"empty query returns top facts by salience), or pass `fact` with `supersedes=<id>` to correct a " +
			"stale fact in one step (upsert — the id comes from a recall or a near-duplicate warning, #606). " +
			"The admin/relate lanes (delete, gc, export, import, consolidate, pin, relate, review) live on the " +
			"CLI (`dex notes`)."),
	}, recordHandler(h))
}

// registerExpertTools wires the power lanes behind DEX_EXPERT (#125):
// deps/graph/refactor/quality tools kept off the everyday surface.
func registerExpertTools(srv *sdk.Server, h toolSurface, td func(string) string, embedAvailable bool) {
	if embedAvailable {
		// Raw ranked hits with the full scoring breakdown (bm25/rrf/lane
		// scores). Everyday concept-search is covered by ask(behavior_search);
		// this power lane exposes the underlying ranking for debugging and
		// precise, filtered queries (#142, demoted from the everyday surface).
		addTool(srv, &sdk.Tool{
			Name:        "search",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Hybrid semantic + BM25 search — raw ranked hits with the full scoring breakdown. " +
				"For everyday concept-search prefer ask (it routes behavior_search and fuses the same lanes); reach " +
				"for search when you need the raw ranking or precise filters. Identifier tokens (CamelCase, " +
				"snake_case, qualified names) are automatically looked up by exact symbol name and fused via " +
				"Reciprocal Rank Fusion — no separate lookup call needed. Supports exclude list, 'languages', and " +
				"'path_glob' filters. On error: 'no-index' (run dex index first), 'embedding-service-unreachable' " +
				"(fall back to grep), or 'ok'."),
		}, h.search)
	}

	// trace + locate: the call-graph and orientation lanes. Demoted from the
	// everyday surface (#143) — everyday agents reach them through `look`
	// (a bare symbol → trace callers, a path:line → locate) and through
	// ask(intent=callers|callees|symbol_lookup|orient). Kept here as direct
	// power lanes for path/impact and precise queries.
	addTool(srv, &sdk.Tool{
		Name:        "trace",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Walk the static call graph from a symbol. `direction`: 'callers' (default — " +
			"who calls it), 'callees' (what it calls), 'path' (shortest call route to the `to` symbol), or " +
			"'impact' (transitive caller blast-radius up to max_depth (default 3): every reachable function with " +
			"its hop depth + PageRank, a risk tier, and `tests_to_run` — the sibling tests of the blast-radius " +
			"files, so change→verify is one call (#654)). " +
			"Go edges are type-resolved; Python/JS/TS/Rust/Java are name-based (tree-sitter) with incomplete " +
			"recall, so an empty result there is not proof of none — verify with grep. Non-empty non-Go " +
			"results are tagged `recall:partial` (callers/callees also fold a grep sweep into `grep_hits`; " +
			"impact just flags the radius as possibly larger). TypeScript additionally resolves constructor-DI " +
			"dispatch — `this.dep.method()` binds to the injected type's method (#85). For a Go method that " +
			"implements a project interface, callers (and impact) also include the INTERFACE-dispatch call sites " +
			"(calls through the interface value), each tagged with `via` naming the interface method — so dynamic " +
			"dispatch isn't missed (#604). Accepts a bare name ('Foo'), " +
			"receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer'). " +
			"Returns 'no-graph' when calls edges haven't been indexed (`dex index . --graph=only`)."),
	}, traceHandler(h))

	addTool(srv, &sdk.Tool{
		Name:        "locate",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("One-call orientation around a code location. Give any one of `ref` " +
			"('path:line'), `symbol` (a name), or `frame` (a raw stack-trace line) and locate resolves " +
			"the enclosing symbol, then returns its callers (+ risk tier), sibling test files, the nearest " +
			"doc (CLAUDE.md/doc.go/README.md walking up), the file's last commit + author, and related " +
			"project notes — in one response. Pass `issues: true` to also list matching open GitHub issues " +
			"via `gh` (best-effort). " +
			"To batch-verify many cited 'file:line[:symbol]' locations in one call (e.g. confirm citations " +
			"from notes/memory still resolve), use the `check` verb instead. " +
			"Pure composition over the index; needs no chat model. Degrades cleanly: " +
			"callers are empty when the graph isn't indexed; returns 'no-index' / 'not-found' otherwise. " +
			"Reach for locate when you already have a concrete path:line, symbol, or panic frame to orient on — " +
			"ask remains the primary entry point for open-ended questions."),
	}, h.locate)

	// The raw primitives the verbs subsume (#145): grep + read (look routes
	// /regex/→grep, path→read), and review_diff (ask("review my changes") covers
	// the everyday worktree case #144 — this is the targeted ref/branch/pr escape
	// hatch). All index-free, so unconditional.
	addTool(srv, &sdk.Tool{
		Name:        "grep",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Exact RE2 regex search over indexed project files — no embedding required. " +
			"Use for literal pattern matches that semantic search misses: cross-cutting symbol references, " +
			"import paths, string literals, regex-sensitive identifiers. Also the primary search lane when " +
			"the embedding service is unavailable. " +
			"Searches the indexed file list when available (respects .gitignore); " +
			"falls back to walking the project directory and skipping .git/vendor/node_modules. " +
			"Returns up to max_results matches (default 50) with path, line number, and trimmed content. " +
			"Pass `context` (1-10) for surrounding lines (like grep -C). " +
			"Pass `fixed:true` to match literally (like grep -F) — no escaping needed for foo.bar, arr[i], f(x). " +
			"Returns 'no-matches' when nothing matches."),
	}, h.searchGrep)

	addTool(srv, &sdk.Tool{
		Name:        "read",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Fetch exact source context for a file you already know. " +
			"Prefer `look(path)` — the fetch verb routes here; use `read` directly for its full mode/slice surface. " +
			"`mode` (default 'full') selects the view: 'full' = raw file content (no LLM, exact bytes); " +
			"'signatures' = indexed symbols + source lines; 'skeleton' = exported type declarations in full plus " +
			"function/method signatures with @B<n> body handles (expand one later via expand='@B<n>'); " +
			"'map' = imports + exported symbols; 'lines:N-M' = raw line slice; " +
			"'analyze' = a token-cost comparison of every mode plus a recommended mode and NO file content, so you " +
			"can pick the cheapest sufficient view before paying to read it; its `handle` (#620) lets you analyze " +
			"many files then lazily expand only the ones you need via read(handle=…, mode=…); " +
			"'summary' = LLM-generated digest (the only mode needing a chat model — pass `focus` to steer, " +
			"e.g. 'public API surface'; returns status='needs-chat' when no chat model is wired). " +
			"Path must resolve inside project_root. Files larger than 64 KB are truncated. " +
			"Pass paths[] (up to 10) to read multiple files in one call — all use the same mode. " +
			"Re-read savings: every response includes `etag` (content hash). On re-reads pass that etag back; " +
			"if the file is unchanged the server returns status=unchanged — reuse the content already in context. " +
			"If the file changed since the last read the server may return status=delta with a compact unified diff " +
			"in Content (saves 40-60% tokens vs re-sending the full file); update your mental model from the diff. " +
			"Pass `task` (your current task from `session`) for automatic compression routing of the raw default. " +
			"Pass `ref` (a git revision: HEAD~5, v1.0, a sha) to time-travel — read the file AS OF that commit, " +
			"with mode=full (raw) or mode=signatures (the historical API); the file must still exist now (#644). " +
			"Any note whose `scope` binds the file is returned in `scoped_notes` (gotcha-on-touch, #645) — read it before you edit. " +
			"Pass `slice` to extract a surgical subset of the content without sending the whole file: " +
			"head:N (first N lines), tail:N (last N lines), range:L1-L2 (1-indexed inclusive), " +
			"search:PATTERN (RE2 grep with ±3 context lines, groups separated by ---), " +
			"json_path:EXPR (dot-path JSON extraction, e.g. $.dependencies). " +
			"Slice composes with handle: the handle resolves to a range first, then slice extracts within it. " +
			"Pass `ccr_hash` (a hex string from a dex:lc_expand:<hash> recovery marker) to retrieve an archived " +
			"tool result from the proxy's CCR tee store; `slice` applies to the retrieved blob. " +
			"On error, returns 'chat-service-unreachable' or 'error'."),
	}, h.summarize)

	addTool(srv, &sdk.Tool{
		Name:        "review_diff",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Per-hunk intelligence for a diff or PR — the targeted-selector review lane " +
			"(ask(\"review my changes\") covers the everyday worktree case, #144). " +
			"Give one of `ref` ('HEAD~3..HEAD' or a single ref vs HEAD), `branch` (what it adds since " +
			"diverging from the default branch), or `pr` (a GitHub PR number, resolved via `gh`). " +
			"For each changed hunk review returns the touched symbols, their callers (+ a risk tier from " +
			"caller blast radius and export status), and related notes; per file it adds sibling tests, the " +
			"nearest doc, 30-day churn, last commit/author, recent author history, and any notes whose " +
			"`scope` binds the file (gotcha-on-touch, #645). " +
			"Pass `compact: true` to drop low-risk hunks. Pure composition over the index; needs no chat " +
			"model. Degrades cleanly: callers/risk are empty when the graph isn't indexed (diff + churn " +
			"still returned); returns 'no-index' / 'no-changes' / 'not-found' otherwise."),
	}, h.review)

	// notes' admin/relate surface (delete, pin/unpin, gc, consolidate,
	// export/import, relate/relations, review) is CLI-only (`dex notes`, #147,
	// #195 S4): record covers the everyday write/recall/supersede hot path, and
	// the admin tail isn't an agent-facing effect. The knowledge engine (h.knowledge)
	// still backs record and the CLI; only the MCP tool is gone.

	// lookup is not a standalone tool — `find` already fuses exact
	// symbol-name hits via RRF, and `ask` detects identifiers and runs
	// the same lookup automatically. The findSymbol handler stays (it
	// backs find's fusion, locate's resolver, and the REST /lookup
	// route); only the redundant tool exposure is removed (#685).
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

	// plan_rename (#638): type-precise rename planner. Go-only, on-demand
	// (no index). Moved to expert: niche workflow, byte-offset input shape
	// that most agents don't construct naturally.
	addTool(srv, &sdk.Tool{
		Name:        "plan_rename",
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

	// rehearse_patch (#730): type-check a hypothetical edit in-memory.
	// Moved to expert: complex byte-range splice input, Go-only, rarely
	// called — agents typically edit then verify_change instead.
	addTool(srv, &sdk.Tool{
		Name:        "rehearse_patch",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Type-check a hypothetical edit in-memory and return new type errors, broken " +
			"files, and tests to run — without writing anything. Closes the chain: " +
			"`plan_rename` (plan) → `rehearse_patch` (prove it compiles) → `Edit` (apply) → `verify_change` (test). " +
			"Pass `edits` as byte-range splices — the same shape `plan_rename` emits (path, start_byte, " +
			"end_byte, replacement), applied highest-offset-first per file. Or pass `files` as whole-file " +
			"replacements (path + contents); `files` takes precedence over `edits` for the same path. " +
			"Returns `compiles` (bool), `diagnostics` (new type errors only — pre-existing errors are " +
			"diffed out), `broken_files` (paths with new errors), and `tests_to_run` (sibling test files). " +
			"Go-only in v1 (returns 'unsupported-language' for non-Go roots); also 'no-edits' when no " +
			"edits/files are supplied. Loads packages on-demand — slower than the read verbs."),
	}, h.rehearse)

	// check (#708): batch citation verification. Moved to expert: meta-tool
	// for QA of prior notes/citations, not primary coding workflow.
	addTool(srv, &sdk.Tool{
		Name:        "check",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Verify a batch of file:line[:symbol] references against the current index — " +
			"use this after making or reviewing changes to confirm that cited locations are still valid. " +
			"Pass `claims` as an array of {ref, symbol?} objects where `ref` is 'file:line', " +
			"'file:line:symbol', or 'file:symbol'. Each result has `status`: " +
			"ok (reference is valid), moved (symbol found at a different line in the same file, " +
			"with `found_at`), gone (symbol/line no longer indexed), no_file (path has no indexed " +
			"chunks), or parse_error (malformed ref). `symbol_at` reports what IS indexed at the " +
			"given line when the expected symbol does not match."),
	}, h.check)

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

	// clones / similar (#84): semantic duplication detection over the
	// vectors already indexed for search. Vector-backed, so only wired
	// when an embedder is present.
	if embedAvailable {
		addTool(srv, &sdk.Tool{
			Name:        "clones",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Find clusters of semantically near-duplicate code blocks (duplication hotspots) — " +
				"the highest-leverage output for review/refactor work, and something grep can't do (it matches " +
				"literals, not meaning). Scans indexed function/method blocks, KNNs each against the rest, and " +
				"union-finds the near-duplicate edges into clusters. Returns clusters of `{path, start_line, " +
				"end_line, kind, name}` with a `similarity` floor and `size`. Args: `path` (restrict to a file/dir " +
				"prefix), `threshold` (min cosine similarity, default 0.90), `min_lines` (default 6), `k`, " +
				"`max_clusters`. Reuses search vectors — no embedder round-trip; an index built without embeddings " +
				"returns none."),
		}, h.clones)

		addTool(srv, &sdk.Tool{
			Name:        "similar",
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
			Description: td("Return code blocks across the repo semantically near a given block, ranked by " +
				"similarity. Point it at a block via `path` + `start_line` (the block indexed at that line); set " +
				"`threshold` (cosine similarity 0..1) to keep only genuine near-duplicates. Use it to answer " +
				"'where else is this logic implemented?' before editing or de-duplicating. Vector KNN over the " +
				"search index — no embedder round-trip."),
		}, h.related)
	}

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

	// refs (#604 Tier 1): type-precise Go symbol queries via go/types.
	// references — all def+use sites; implementations — concrete types
	// satisfying an interface; supertypes — embedded interfaces / interfaces
	// a type satisfies; subtypes — implementing types / embedding structs.
	addTool(srv, &sdk.Tool{
		Name:        "refs",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Type-precise Go symbol queries via go/types — no index needed; Go-only. " +
			"Give a `symbol` (bare 'Foo', receiver-qualified '(*Server).Run', or pkg-qualified 'mcp.NewServer') " +
			"and an `action`: " +
			"'references' (all def + use sites across the module), " +
			"'implementations' (concrete types satisfying an interface), " +
			"'supertypes' (interfaces embedded by an interface, or interfaces a concrete type satisfies within the module), " +
			"'subtypes' (types implementing an interface, or structs embedding a struct). " +
			"Returns a list of {path, line, kind} sites. Returns 'unsupported-language' for non-Go. " +
			"For interface implementors, `cohort` gives richer coverage-gap analysis; refs gives the raw query."),
	}, h.refs)

	// `path` is not a standalone tool — `trace --dir path --to <dst>`
	// finds the shortest route between two symbols (#575).
	// `diff` removed: review_diff + trace direction=impact cover blast-radius
	// from changed files. `budget` removed: session action=budget covers it.

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
		Name:        "repo_map",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Deterministic, multi-zoom topology map of the project's top packages/dirs " +
			"and how they connect — no embedding or chat required. " +
			"Use for structural exploration when you need raw topology rather than a task context pack. " +
			"For coding tasks, call `ask(task, intent=assemble)` instead — it returns ranked files and orientation together. " +
			"Returns 'no-index' when the project hasn't been indexed yet."),
	}, mapHandler(h))

	addTool(srv, &sdk.Tool{
		Name:        "status",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("Report dex endpoint health and the list of indexed projects with their chunk counts and last-indexed times. " +
			"For everyday use, single-project index freshness is embedded in `ask` responses — call this for cross-project health checks or debugging."),
	}, h.status)

	// The session/checkpoint admin surface (set_task/add_note/snapshot/export/
	// import/budget/heatmap) was removed with the two-verb collapse (#195 S4):
	// running a working session is the harness's job, not an advisory effect.
	// The dedup that made it useful stays *internal* — sessionAutoFile tracks
	// touched files and the seen/delta mechanism suppresses re-sends, and the
	// current task still surfaces in query responses as session_task.
}

// registerQueryTool wires the single read verb of the two-verb surface (#196,
// epic #195, spec specs/two-verb-surface.md). query merges the four-verb `ask`
// (infer intent from a question) and `look` (exact fetch of a named target) into
// one classifier over the same engine — output precision tracks input precision.
// It BM25-falls-back on its own when no embedder is wired.
func registerQueryTool(srv *sdk.Server, h toolSurface, td func(string) string) {
	addTool(srv, &sdk.Tool{
		Name:        "query",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		Description: td("The read verb — one call to read the codebase intelligence. Its input SHAPE picks " +
			"the lane and the answer's precision: a file path ('internal/mcp/server.go') → its compressed " +
			"signatures (raw bytes are the native Read tool's job), a `path:line` " +
			"('server.go:829') or range ('server.go:120-140') → that slice, a `/regex/` ('/func .*Verb/') → grep, a bare symbol ('NewServer', " +
			"'(*Server).Run', 'mcp.NewServer') → JUST its call graph, and a prose question ('how are edits " +
			"debounced?') → a ranked semantic evidence pack (semantic_hits + symbols + suggested_reads, contents " +
			"inlined). A named symbol earns a precise (graph) answer, never a fuzzy one — that is the narrow " +
			"default. Force the lane with `kind` (read|grep|locate|symbol|callers|callees|impact|path|search|" +
			"editing|assemble|architecture|packages|orient|review) and the facet with `want`. The envelope's " +
			"`route` echoes the shape it detected and the alternative it did not take; `trust` carries per-result " +
			"provenance (exact | semantic | name-based); `next` offers the road not taken. Empty input returns the " +
			"session-start orientation map."),
	}, queryHandler(h))
}

// Version is the build version. A release build overrides it via
// -ldflags "-X .../internal/mcp.Version=<v>" (mooncake task install). When it
// is still "dev" — e.g. a plain `go install` — resolveVersion (version.go)
// recovers the VCS revision Go embeds in the build info.
var Version = "dev"
