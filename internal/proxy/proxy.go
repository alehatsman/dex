// Package proxy is the loopback Anthropic API pass-through (epic #232).
//
// It sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL,
// running each /v1/messages request through two deterministic passes:
//  1. PruneRequestBody — rewrites old tool_result blocks outside keep_recent.
//  2. CompressRequestBody — entropy + terse compression on all tool_results.
//
// Token counts are measured before and after, accumulated in Stats, and
// exposed via GET /stats (JSON Snapshot). Run dex proxy --stats to fetch it.
//
// Posture mirrors `dex serve`: loopback bind only (the proxy sees the agent's
// API key, so it must never be exposed on the network), and request bodies are
// never logged. Ported in spirit from lean-ctx rust/src/proxy/{mod,forward,metrics}.rs.
package proxy

import (
	"context"
	"encoding/json"
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
	// Stats accumulates per-session token counters. Nil = allocate fresh.
	// The same pointer is used by the /stats endpoint so callers can observe
	// it after Run returns.
	Stats *Stats
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
	if opts.Stats == nil {
		opts.Stats = &Stats{}
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

	handler := newProxyHandler(upstreamURL, opts.Logger, opts.Stats)

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

// FetchStats fetches a Snapshot from a running proxy at addr (host:port).
func FetchStats(ctx context.Context, addr string) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/stats", nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("proxy /stats returned %d", resp.StatusCode)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode /stats: %w", err)
	}
	return snap, nil
}

// newProxyHandler builds the mux:
//   - GET /stats → JSON Snapshot (no PII, no bodies)
//   - everything else → compress + forward to upstream
func newProxyHandler(upstream *url.URL, logger *slog.Logger, stats *Stats) http.Handler {
	rp := &httputil.ReverseProxy{
		// FlushInterval -1 flushes each chunk as it arrives — mandatory for
		// SSE so the agent sees tokens stream rather than waiting on a buffer.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			// Drop X-Forwarded-* — transparent loopback hop, not an edge proxy.
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		// Fail open: upstream errors must not crash the proxy.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			logger.Warn("dex proxy forward error", "method", r.Method, "path", r.URL.Path, "err", e)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /stats — JSON snapshot of cumulative token counters (no PII).
		if r.Method == http.MethodGet && r.URL.Path == "/stats" {
			snap := stats.Snapshot()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snap)
			return
		}

		// POST /v1/messages — prune + compress, then forward.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages") {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				logger.Warn("dex proxy: request body read failed; forwarding may be incomplete", "err", err)
				r.Body = io.NopCloser(strings.NewReader(""))
				rp.ServeHTTP(w, r)
				return
			}

			before := countBodyTokens(body)
			current := body
			var paths []string

			pruned, prunedBytes := PruneRequestBody(current, DefaultKeepRecent)
			if prunedBytes > 0 {
				current = pruned
				paths = append(paths, "prune")
			}

			compressed, compressedBytes := CompressRequestBody(current)
			if compressedBytes > 0 {
				current = compressed
				paths = append(paths, "compress")
			}

			after := countBodyTokens(current)
			stats.record(before, after)
			logRequestMetrics(logger, r, current, before, after, paths)

			r.Body = io.NopCloser(strings.NewReader(string(current)))
			r.ContentLength = int64(len(current))
		}

		// Everything else — forward verbatim.
		rp.ServeHTTP(w, r)
	})
}

// validateLoopback rejects any bind that isn't explicitly loopback. The proxy
// forwards the agent's API key upstream, so it must never listen on a routable
// interface.
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
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("addr %q binds to %s (non-loopback); the proxy is loopback-only, use 127.0.0.1:<port>", addr, host)
}
