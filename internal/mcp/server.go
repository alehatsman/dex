// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/retrieve"
	"github.com/alehatsman/dex/internal/slo"
	"github.com/alehatsman/dex/internal/store"
	"github.com/alehatsman/dex/internal/throttle"
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
