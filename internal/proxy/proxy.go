// Package proxy is the spike for the conversation-history compression seam
// (epic #232): a loopback-only HTTP server that sits between Claude Code and
// the Anthropic API at ANTHROPIC_BASE_URL and forwards /v1/messages verbatim.
//
// This cut does NO compression. It proves the seam works end-to-end —
// transparent request/response forwarding including SSE streaming passthrough,
// fail-open on any of the proxy's own errors — and logs a per-request
// input-token baseline so the follow-up tickets (#237 history pruning, #238
// tool_result compression) can show a before/after delta against real traffic.
//
// Posture mirrors `dex serve`: loopback bind only (the proxy sees the agent's
// API key, so it must never be network-exposed), and request bodies are never
// logged. Ported in spirit from lean-ctx rust/src/proxy/{mod,forward}.rs.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// DefaultUpstream is the real Anthropic API, used when --upstream is unset.
const DefaultUpstream = "https://api.anthropic.com"

// Options configures Run.
type Options struct {
	// Addr is the loopback listen address (e.g. "127.0.0.1:8788"). A
	// non-loopback bind is rejected at startup — the proxy handles API keys
	// and must never be exposed on the network.
	Addr string
	// Upstream is the real API base URL requests are forwarded to. Defaults
	// to DefaultUpstream when empty.
	Upstream string
	// Logger receives structured per-request logs. Nil = discard.
	Logger *slog.Logger
}

// Run starts the loopback proxy and blocks until ctx is cancelled or the
// listener fails. It binds synchronously so a bind error surfaces to the
// caller rather than being buried in a goroutine.
func Run(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if strings.TrimSpace(opts.Upstream) == "" {
		opts.Upstream = DefaultUpstream
	}
	if err := validateLoopback(opts.Addr); err != nil {
		return err
	}
	upstreamURL, err := url.Parse(opts.Upstream)
	if err != nil {
		return fmt.Errorf("parse upstream %q: %w", opts.Upstream, err)
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return fmt.Errorf("upstream %q must be an absolute URL (scheme://host)", opts.Upstream)
	}

	handler := newProxyHandler(upstreamURL, opts.Logger)

	httpSrv := &http.Server{
		Addr:              opts.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	listener, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Addr, err)
	}
	opts.Logger.Info("dex proxy listening",
		"addr", listener.Addr().String(),
		"upstream", upstreamURL.String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

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

// newProxyHandler builds the reverse proxy that forwards everything to
// upstream verbatim. SSE responses stream through unbuffered (FlushInterval
// = -1 flushes each write immediately); request bodies are tee'd for a
// best-effort token-count baseline that never blocks forwarding.
func newProxyHandler(upstream *url.URL, logger *slog.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		// FlushInterval -1 flushes each chunk as it arrives — mandatory for
		// SSE so the agent sees tokens stream rather than waiting on a buffer.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			// Drop X-Forwarded-* — this is a transparent loopback hop, not an
			// edge proxy, and leaking 127.0.0.1 upstream is noise at best.
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		// Fail open: an upstream/transport error must never surface as a
		// proxy-authored crash. Log it and return a bare 502 — there is no
		// upstream response to "pass through" when the hop itself failed.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			logger.Warn("dex proxy forward error", "method", r.Method, "path", r.URL.Path, "err", e)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// On /v1/messages: read body, prune old tool_result history (fail-open),
		// log input-token baseline on the pruned body, then forward.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages") {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				logger.Warn("dex proxy: request body read failed; forwarding may be incomplete", "err", err)
				r.Body = io.NopCloser(strings.NewReader(""))
				rp.ServeHTTP(w, r)
				return
			}
			pruned, prunedBytes := PruneRequestBody(body, DefaultKeepRecent)
			if prunedBytes > 0 {
				logger.Info("dex proxy prune", "saved_bytes", prunedBytes)
			}
			compressed, compressedBytes := CompressRequestBody(pruned)
			if compressedBytes > 0 {
				logger.Info("dex proxy compress", "saved_bytes", compressedBytes)
			}
			r.Body = io.NopCloser(strings.NewReader(string(compressed)))
			r.ContentLength = int64(len(compressed))
			logInputBaseline(logger, r, compressed)
		}
		rp.ServeHTTP(w, r)
	})
}

// validateLoopback rejects any bind that isn't explicitly loopback. The proxy
// forwards the agent's API key upstream, so it must never listen on a routable
// interface. Conservative by design — same posture as `dex serve` without a
// token, with no token escape hatch since there is nothing to authenticate.
func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q must be host:port (e.g. 127.0.0.1:8788): %w", addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	case "":
		return fmt.Errorf("addr %q binds to all interfaces; use 127.0.0.1:<port> (the proxy is loopback-only)", addr)
	}
	// A literal loopback IP in 127.0.0.0/8 is fine; anything else is rejected.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("addr %q binds to %s (non-loopback); the proxy is loopback-only, use 127.0.0.1:<port>", addr, host)
}
