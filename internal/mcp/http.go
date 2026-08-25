package mcp

// HTTP transport for the same primitives the stdio MCP server
// exposes — `Server.RunHTTP` mounts a REST surface that lets coding
// agents and other services hit dex over the network instead of via
// MCP's JSON-RPC stdio. Same Server struct, same tool handlers, just
// a different wire protocol.
//
// Endpoint layout, all under /v1:
// GET  /healthz                    — liveness; never authenticated
// GET  /version                    — build version; never authenticated
// GET  /projects                   — list registered (id, root, db_path)
// GET  /status                     — global health + indexed projects
// POST /shell                      — body: ShellInput
// POST /projects/{id}/ask          — body: ContextInput
// POST /projects/{id}/map          — body: MapInput
// POST /projects/{id}/trace        — body: TraceInput
// POST /projects/{id}/find         — body: SearchInput
// POST /projects/{id}/lookup       — body: FindSymbolInput
// POST /projects/{id}/grep         — body: SearchGrepInput
// POST /projects/{id}/read         — body: SummarizeInput
// POST /projects/{id}/ls           — body: SearchTreeInput
// GET  /projects/{id}/graph/packages — whole pkg import DAG
// POST /projects/{id}/deps         — body: GraphDepsInput
// POST /projects/{id}/callers      — body: CallEdgeInput
// POST /projects/{id}/callees      — body: CallEdgeInput
// POST /projects/{id}/impact       — body: ImpactInput
// POST /projects/{id}/routes       — body: RoutesInput
// POST /projects/{id}/smells       — body: SmellsInput
// POST /projects/{id}/clones       — body: ClonesInput
// POST /projects/{id}/refs         — body: RefsInput
// POST /projects/{id}/path         — body: PathInput
// POST /projects/{id}/diff         — body: DiffInput
// POST /projects/{id}/clusters     — body: CommunitiesInput
// POST /projects/{id}/notes        — body: KnowledgeInput
// POST /projects/{id}/session      — body: SessionInput
// *    /projects/{id}/mcp          — native streamable-HTTP MCP
//                                    transport (http_mcp.go), not REST
//
// The URL's {id} resolves to a project root via the operator-provided
// registry (RunHTTPOptions.Projects). The corresponding Input struct's
// project field is always overridden with the registry value, so a
// client can't smuggle a different path via the body.
//
// Auth: when RunHTTPOptions.Token is non-empty, every authenticated
// route requires `Authorization: Bearer <token>`. Mismatched or
// missing token → 401. When Token is empty, the server refuses to
// bind anywhere outside loopback (a misconfigured `--addr 0.0.0.0:X`
// without a token is rejected at startup).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/logx"
	"github.com/alehatsman/dex/internal/proj"
)

// ProjectEntry registers one project the daemon will serve. ID is
// derived from sha256(realpath(Root)) — same scheme proj.Resolve uses
// — so URLs computed by clients line up with cache directory names.
type ProjectEntry struct {
	ID   string // sha256-hex
	Root string // absolute, EvalSymlinks-resolved
}

// RunHTTPOptions configures Server.RunHTTP. The operator hands in the
// project registry; the server doesn't auto-discover for the v1 cut.
type RunHTTPOptions struct {
	// Addr is the listen address. ":8080" listens on all interfaces;
	// "127.0.0.1:8080" listens on loopback only. When Token is empty
	// and Addr resolves to a non-loopback bind, RunHTTP returns an
	// error rather than starting an unauthenticated public listener.
	Addr string
	// Token, when non-empty, is the bearer token required on every
	// authenticated route. Compare against the Authorization header's
	// "Bearer X" payload (constant-time). Empty Token = loopback-only.
	Token string
	// Projects maps a project id (sha256 of realpath) to its absolute
	// root path. The HTTP layer trusts this map; clients can only
	// reach roots that appear here.
	Projects map[string]string
	// Logger receives structured access logs. Nil = discard.
	Logger *slog.Logger
	// ReadHeaderTimeout is the maximum time spent reading request
	// headers. Defaults to 5s when zero.
	ReadHeaderTimeout time.Duration
	// EagerWatch, when true (and AutoWatch is enabled), spawns a watcher
	// for every project in the registry at startup rather than lazily on
	// the first query. Lets `dex serve` be the single eager re-index
	// watcher on the server box, so the dedicated dex-watch@ units can be
	// retired. Idempotent with the lazy path: a later query touching the
	// same project is a no-op (ensureWatcher dedupes on project ID).
	EagerWatch bool
}

// ProjectID computes the canonical project ID from a filesystem path.
// Mirrors what proj.Resolve writes into Project.ID: sha256 of the
// EvalSymlinks-resolved absolute path. Returns ("", err) when the
// path doesn't exist or can't be resolved.
func ProjectID(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(real))
	return hex.EncodeToString(sum[:]), nil
}

// BuildProjectRegistry resolves a list of operator-supplied project
// roots into the (id → realpath) map RunHTTPOptions.Projects expects.
// Returns an error on the first invalid path; the caller decides
// whether to skip-and-warn or fail-fast.
func BuildProjectRegistry(roots []string) (map[string]string, error) {
	out := make(map[string]string, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", r, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", r, err)
		}
		sum := sha256.Sum256([]byte(real))
		id := hex.EncodeToString(sum[:])
		out[id] = real
	}
	return out, nil
}

// RunHTTP starts the HTTP transport on opts.Addr. Blocks until ctx
// is cancelled or the listener fails; graceful-shutdowns on
// cancellation with a 5s drain budget.
func (s *Server) RunHTTP(ctx context.Context, opts RunHTTPOptions) error {
	if err := validateBindForAuth(opts.Addr, opts.Token); err != nil {
		return err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opts.Logger = opts.Logger.With("subsystem", "mcp")
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 5 * time.Second
	}
	// runCtx lets handlers reuse the same lifecycle path the stdio
	// transport uses — autowatchers spawned during HTTP serving will
	// drain cleanly when ctx is cancelled.
	s.runCtx = ctx

	handler := s.buildHTTPHandler(opts)

	httpSrv := &http.Server{
		Addr:              opts.Addr,
		Handler:           handler,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}

	// Listen up-front so binding errors are visible synchronously,
	// not buried in a goroutine.
	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Addr, err)
	}
	opts.Logger.Info("dex serve listening",
		"addr", listener.Addr().String(),
		"projects", len(opts.Projects),
		"auth", opts.Token != "")

	// Spawn watchers for the whole registry up-front (idempotent with the
	// lazy on-query path). Done after a successful Listen so a bind error
	// returns before any goroutine is started.
	if opts.EagerWatch {
		s.startEagerWatchers(opts.Projects, opts.Logger)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	defer s.watcherWG.Wait()
	defer s.waitSessionWrites() // flush pending session records before returning
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// startEagerWatchers resolves every registry root to a Project and spawns
// its watcher immediately, instead of waiting for the first query to do so
// via resolveProject. No-op unless AutoWatch is enabled (ensureWatcher's
// own guard); a resolve failure for one root warns and skips rather than
// aborting startup, mirroring cmdServe's PreflightProjects loop. Safe to
// call alongside the lazy path — ensureWatcher dedupes on project ID.
func (s *Server) startEagerWatchers(projects map[string]string, logger *slog.Logger) {
	if !s.AutoWatch.Enabled {
		return
	}
	for _, root := range projects {
		p, err := proj.Resolve(root, s.IndexDir)
		if err != nil {
			logger.Warn("dex serve: eager watch skipped", "root", root, "err", err)
			continue
		}
		s.ensureWatcher(p)
	}
}

// buildHTTPHandler wires up the mux + middleware chain that RunHTTP
// installs. Extracted so tests can wrap it with httptest.NewServer
// without going through bind/listen.
func (s *Server) buildHTTPHandler(opts RunHTTPOptions) http.Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	mux := http.NewServeMux()

	// Unauthenticated routes.
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": Version})
	})

	// Authenticated routes. Mounted on a sub-mux so a single
	// authMiddleware wrapper applies to all of them.
	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/projects", s.handleListProjects(opts.Projects))
	authed.HandleFunc("GET /v1/status", s.handleStatus)
	authed.HandleFunc("POST /v1/projects/{id}/ask", s.handleAsk(opts.Projects))
	authed.HandleFunc("POST /v1/projects/{id}/map", jsonHandler(opts.Projects, func(in *MapInput, r string) { in.ProjectRoot = r }, s.Map))
	authed.HandleFunc("POST /v1/projects/{id}/trace", jsonHandler(opts.Projects, func(in *TraceInput, r string) { in.ProjectRoot = r }, s.Trace))
	authed.HandleFunc("POST /v1/projects/{id}/locate", jsonHandler(opts.Projects, func(in *LocateInput, r string) { in.ProjectRoot = r }, s.Locate))
	authed.HandleFunc("POST /v1/projects/{id}/review", jsonHandler(opts.Projects, func(in *ReviewInput, r string) { in.ProjectRoot = r }, s.Review))
	authed.HandleFunc("POST /v1/projects/{id}/refactor", jsonHandler(opts.Projects, func(in *RefactorInput, r string) { in.ProjectRoot = r }, s.Refactor))
	authed.HandleFunc("POST /v1/projects/{id}/cohort", jsonHandler(opts.Projects, func(in *CohortInput, r string) { in.ProjectRoot = r }, s.Cohort))
	authed.HandleFunc("POST /v1/projects/{id}/find", jsonHandler(opts.Projects, func(in *SearchInput, r string) { in.ProjectRoot = r }, s.Search))
	authed.HandleFunc("POST /v1/projects/{id}/lookup", jsonHandler(opts.Projects, func(in *FindSymbolInput, r string) { in.ProjectRoot = r }, s.FindSymbol))
	authed.HandleFunc("POST /v1/projects/{id}/grep", jsonHandler(opts.Projects, func(in *SearchGrepInput, r string) { in.ProjectRoot = r }, s.SearchGrep))
	authed.HandleFunc("POST /v1/projects/{id}/read", jsonHandler(opts.Projects, func(in *SummarizeInput, r string) { in.ProjectRoot = r }, s.Summarize))
	authed.HandleFunc("POST /v1/projects/{id}/ls", jsonHandler(opts.Projects, func(in *SearchTreeInput, r string) { in.ProjectRoot = r }, s.SearchTree))
	authed.HandleFunc("GET /v1/projects/{id}/graph/packages", jsonHandler(opts.Projects, func(in *PackageGraphInput, r string) { in.ProjectRoot = r }, s.PackageGraph))
	authed.HandleFunc("POST /v1/projects/{id}/deps", jsonHandler(opts.Projects, func(in *GraphDepsInput, r string) { in.ProjectRoot = r }, s.GraphDeps))
	authed.HandleFunc("POST /v1/projects/{id}/callers", jsonHandler(opts.Projects, func(in *CallEdgeInput, r string) { in.ProjectRoot = r }, s.GraphCallers))
	authed.HandleFunc("POST /v1/projects/{id}/callees", jsonHandler(opts.Projects, func(in *CallEdgeInput, r string) { in.ProjectRoot = r }, s.GraphCallees))
	authed.HandleFunc("POST /v1/projects/{id}/impact", jsonHandler(opts.Projects, func(in *ImpactInput, r string) { in.ProjectRoot = r }, s.GraphImpact))
	authed.HandleFunc("POST /v1/projects/{id}/routes", jsonHandler(opts.Projects, func(in *RoutesInput, r string) { in.ProjectRoot = r }, s.Routes))
	authed.HandleFunc("POST /v1/projects/{id}/smells", jsonHandler(opts.Projects, func(in *SmellsInput, r string) { in.ProjectRoot = r }, s.Smells))
	authed.HandleFunc("POST /v1/projects/{id}/clones", jsonHandler(opts.Projects, func(in *ClonesInput, r string) { in.ProjectRoot = r }, s.Clones))
	authed.HandleFunc("POST /v1/projects/{id}/refs", jsonHandler(opts.Projects, func(in *RefsInput, r string) { in.ProjectRoot = r }, s.Refs))
	authed.HandleFunc("POST /v1/projects/{id}/path", jsonHandler(opts.Projects, func(in *PathInput, r string) { in.ProjectRoot = r }, s.GraphPath))
	authed.HandleFunc("POST /v1/projects/{id}/diff", jsonHandler(opts.Projects, func(in *DiffInput, r string) { in.ProjectRoot = r }, s.GraphDiff))
	authed.HandleFunc("POST /v1/projects/{id}/clusters", jsonHandler(opts.Projects, func(in *CommunitiesInput, r string) { in.ProjectRoot = r }, s.GraphCommunities))
	authed.HandleFunc("POST /v1/projects/{id}/notes", jsonHandler(opts.Projects, func(in *KnowledgeInput, r string) { in.ProjectRoot = r }, s.Knowledge))
	authed.HandleFunc("POST /v1/projects/{id}/index-status", jsonHandler(opts.Projects, func(in *IndexStatusInput, r string) { in.ProjectRoot = r }, s.IndexStatus))

	// Native streamable-HTTP MCP transport — clients attach dex directly over
	// MCP at /v1/projects/{id}/mcp (no stdio shim). Mounted method-agnostic:
	// the streamable protocol uses POST (messages), GET (SSE stream), and
	// DELETE (session end). Same bearer auth via the authed submux.
	if mcpHandler := s.newMCPHandler(opts.Projects); mcpHandler != nil {
		authed.Handle("/v1/projects/{id}/mcp", mcpHandler)
	}

	wrapped := authMiddleware(opts.Token, authed)
	// Forward the whole /v1/ subtree to the authenticated submux. The
	// unauthenticated GET /v1/healthz and GET /v1/version are more specific
	// patterns, so Go 1.22's ServeMux routes them to the bare mux first; every
	// other /v1/* route (/v1/projects, /v1/status, /v1/shell, the per-project
	// short-name tools) lands on the authed submux.
	mux.Handle("/v1/", wrapped)

	return recoverMiddleware(logger, logMiddleware(logger, mux))
}

// validateBindForAuth refuses to start a no-token server on a
// non-loopback address. The check is conservative — anything that
// isn't explicitly 127.0.0.1, [::1], or localhost trips it. Operators
// who really want anonymous network exposure can set
// DEX_SERVE_TOKEN to a known-public placeholder, but the friction is
// intentional.
func validateBindForAuth(addr, token string) error {
	if token != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Bare ":8080" form — host is empty, which means "all
		// interfaces". Require a token in that shape.
		return fmt.Errorf("addr %q binds to all interfaces; set DEX_SERVE_TOKEN or use 127.0.0.1:<port>", addr)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost", "":
		if host == "" {
			return fmt.Errorf("addr %q binds to all interfaces; set DEX_SERVE_TOKEN or use 127.0.0.1:<port>", addr)
		}
		return nil
	}
	return fmt.Errorf("addr %q binds to %s (non-loopback); set DEX_SERVE_TOKEN", addr, host)
}

// ─── middleware ─────────────────────────────────────────────────────────────

// authMiddleware enforces bearer-token auth when token != "". An
// empty token means "no auth" and is only safe in combination with
// validateBindForAuth's loopback check (enforced at startup).
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expect := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if !constantTimeEqual(got, expect) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware turns a handler panic into a 500 + log entry.
// The actual stack stays in the log; the wire response is generic so
// internal details don't leak to callers.
func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic in handler", "path", r.URL.Path, "method", r.Method, "rec", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logMiddleware writes a structured access log per request. method,
// path, status, duration, peer.
func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			logx.DurMS(time.Since(start)),
			"remote", r.RemoteAddr)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// constantTimeEqual compares two strings in constant time using crypto/subtle.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ─── shared handler helpers ─────────────────────────────────────────────────

// resolveProjectFromURL looks up the {id} path segment against the
// operator's registry. Writes the appropriate error response and
// returns "" when not found.
func resolveProjectFromURL(w http.ResponseWriter, r *http.Request, projects map[string]string) string {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return ""
	}
	canonical, ok, ambiguous := resolveRegistryID(id, projects)
	if ambiguous {
		writeError(w, http.StatusBadRequest, "ambiguous project id prefix: "+id)
		return ""
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown project id: "+id)
		return ""
	}
	return projects[canonical]
}

// resolveRegistryID maps a URL {id} to a canonical registry key, accepting
// either the full key or an unambiguous prefix of it. The boot banner
// prints a 12-char id prefix (serve.go), so an operator can paste that
// straight into a REST/MCP-over-HTTP call. An exact match always wins; a
// prefix that matches exactly one key resolves to it; a prefix matching
// more than one key is reported ambiguous (the caller turns that into an
// error rather than silently picking one).
func resolveRegistryID[V any](id string, registry map[string]V) (canonical string, ok bool, ambiguous bool) {
	if _, exact := registry[id]; exact {
		return id, true, false
	}
	var match string
	n := 0
	for k := range registry {
		if strings.HasPrefix(k, id) {
			match, n = k, n+1
		}
	}
	switch n {
	case 1:
		return match, true, false
	case 0:
		return "", false, false
	default:
		return "", false, true
	}
}

// decodeBody reads the request body into v. Empty bodies (Content-
// Length 0, no body) are accepted — most Inputs have only optional
// fields beyond the URL-supplied project id.
func decodeBody(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)) // 1 MiB cap
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// writeJSON serializes v as the response body with the given status.
// Errors during encoding are logged via the recorded status; we
// can't recover the wire state cleanly past WriteHeader.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleListProjects(projects map[string]string) http.HandlerFunc {
	type entry struct {
		ID   string `json:"id"`
		Root string `json:"root"`
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		out := make([]entry, 0, len(projects))
		for id, root := range projects {
			out = append(out, entry{ID: id, Root: root})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
		writeJSON(w, http.StatusOK, map[string]any{"projects": out})
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out, err := s.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAsk(projects map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := resolveProjectFromURL(w, r, projects)
		if root == "" {
			return
		}
		var in ContextInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		in.ProjectRoot = root
		_, out, err := s.ContextRouter(r.Context(), in)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// jsonHandler builds the HTTP handler shared by every project-scoped tool:
// resolve {id} to a project root, decode the JSON body into In, override its
// project field via bind (so a client can't smuggle a different path), invoke
// call, and encode the result. The handlers below differ only in their input
// type and service method, so they collapse to thin delegators.
func jsonHandler[In, Out any](
	projects map[string]string,
	bind func(in *In, root string),
	call func(ctx context.Context, in In) (Out, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := resolveProjectFromURL(w, r, projects)
		if root == "" {
			return
		}
		var in In
		if err := decodeBody(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		bind(&in, root)
		out, err := call(r.Context(), in)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// ─── package-level helpers exposed for cmd/dex/serve.go ─────────────────────

// PreflightProjects validates that each operator-supplied root has
// an index on disk. Missing indexes don't block startup — the daemon
// will still serve them and return no-index responses per call —
// but the returned warnings give the operator a clear list to act on.
func PreflightProjects(projects map[string]string, indexDir string) (warnings []string) {
	for id, root := range projects {
		p, err := proj.Resolve(root, indexDir)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("project %s (%s): resolve: %v", id, root, err))
			continue
		}
		if !fileExists(p.DBPath) {
			warnings = append(warnings, fmt.Sprintf("project %s (%s): no index — run `dex index %s`", id, root, root))
		}
	}
	return warnings
}

// fileExists checks whether a path exists on disk. Used only by
// PreflightProjects to flag missing indexes at startup.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
