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
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/alehatsman/dex/internal/logx"
)

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
