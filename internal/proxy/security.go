package proxy

// Security posture for the loopback proxy (#240). The proxy terminates the
// agent↔API path and forwards the agent's Anthropic credential upstream, so it
// is locked down to the same posture as `dex serve`:
//
//   - Loopback bind only by default. A non-loopback --addr is refused unless
//     DEX_PROXY_TOKEN is set (mirrors DEX_SERVE_TOKEN's friction).
//   - When a token is set, incoming requests must carry it in the
//     X-Dex-Proxy-Token header (a DISTINCT header from the upstream credential,
//     so gating never clobbers the Authorization / x-api-key passthrough).
//   - The gate header is stripped before forwarding upstream.
//   - Request/response bodies are never logged (enforced in metrics.go); the
//     upstream credential is passed through untouched and never persisted.

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
)

// ProxyTokenHeader carries the proxy's own auth token on incoming requests.
// It is deliberately NOT Authorization / x-api-key — those belong to the
// upstream Anthropic credential, which the proxy forwards verbatim. Using a
// separate header lets the gate coexist with credential passthrough.
const ProxyTokenHeader = "X-Dex-Proxy-Token"

// validateBind refuses to start a no-token proxy on a non-loopback address.
// Mirrors dex serve's validateBindForAuth: loopback binds (127.0.0.0/8, ::1,
// localhost) are always allowed; anything else requires DEX_PROXY_TOKEN so
// network exposure of an API-key-handling proxy is always a conscious opt-in.
func validateBind(addr, token string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q must be host:port (e.g. 127.0.0.1:8788): %w", addr, err)
	}
	if token != "" {
		// An explicit token gate is set — a non-loopback bind is a conscious
		// opt-in. (Loopback + token is fine too.)
		return nil
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	case "":
		return fmt.Errorf("addr %q binds to all interfaces; set DEX_PROXY_TOKEN or use 127.0.0.1:<port> (the proxy handles API keys)", addr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("addr %q binds to %s (non-loopback); set DEX_PROXY_TOKEN or use 127.0.0.1:<port> (the proxy handles API keys)", addr, host)
}

// authGate enforces the X-Dex-Proxy-Token header when token != "". An empty
// token means "no gate", which is only safe paired with validateBind's
// loopback check (enforced at startup). The token is compared in constant time
// and never logged.
func authGate(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(ProxyTokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			// Generic message — never echo the supplied value.
			http.Error(w, "missing or invalid "+ProxyTokenHeader, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
