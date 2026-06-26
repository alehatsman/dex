// Package proxy is the loopback Anthropic API pass-through (epic #232).
//
// It sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL,
// running each /v1/messages request through deterministic passes that leave
// tool_result CONTENT untouched — the model always sees verbatim tool output:
//  0. RouteModel — rewrites the "model" field based on input token count (opt-in).
//  1. PruneRequestBody — rewrites old tool_result blocks (outside keep_recent)
//     to compact re-read stubs; recent results pass through verbatim (#357).
//  2. tool-description compression + cache-breakpoint alignment.
//
// Token counts are measured before and after, accumulated in Stats, and
// exposed via GET /stats (JSON Snapshot). Run dex proxy --stats to fetch it.
//
// Posture mirrors `dex serve` (see security.go): loopback bind only by default
// — the proxy sees the agent's API key, so it is never exposed on the network
// unless DEX_PROXY_TOKEN is set, which then gates incoming requests via the
// X-Dex-Proxy-Token header. Request/response bodies are never logged and the
// upstream credential is forwarded untouched, never persisted. Ported in spirit
// from lean-ctx rust/src/proxy/{mod,forward,metrics}.rs.
package proxy

import (
	"bytes"
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
	// Token, when non-empty, gates incoming requests via the X-Dex-Proxy-Token
	// header and permits a non-loopback bind. Empty = loopback-only, no gate
	// (same posture as DEX_SERVE_TOKEN on `dex serve`).
	Token string
	// ToolDescMode selects how aggressively MCP tool descriptions are
	// rewritten in flight (see toolcompress.go). Zero value is ToolDescFull
	// (no-op), the conservative default.
	ToolDescMode ToolDescMode
	// RouteConfig drives token-count-based model routing (see modelroute.go).
	// Zero value (Enabled:false) disables routing — requests pass through with
	// the model field untouched.
	RouteConfig ModelRouteConfig
	// EditFailHook, when non-nil, is called once per detected edit-fail event
	// (#58) with the file path that triggered the signal. Callers use this to
	// forward the event to adaptive.PolicyTable.RecordSignal.
	EditFailHook func(path string)
	// BudgetLog, when non-nil, records per-turn and per-compact budget events
	// to the session JSONL log (#60).
	BudgetLog *BudgetLog
	// Pricing is the model → cost table used to compute SessionCostUSD (#56).
	// Nil uses LoadPricing() (defaultPricing + DEX_MODEL_PRICING_JSON overrides).
	Pricing map[string]ModelPricing
	// CostHook, when non-nil, is called after each response with the incremental
	// USD cost for that response. Callers use this to forward cost to an SLO tracker.
	CostHook func(costUSD float64)
	// Effort, when non-empty, injects a reasoning-budget hint into every outbound
	// request that doesn't already set one. See ParseEffortLevel (#30).
	Effort EffortLevel
	// CCREnabled turns on content-addressed recovery (#597): pruned file-read
	// results are teed to a content-addressed store and stubs carry a recovery
	// marker, and the keep-window expansion pass runs pre-send. Off by default.
	CCREnabled bool
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
	if opts.Pricing == nil {
		opts.Pricing = LoadPricing()
	}
	if err := validateBind(opts.Addr, opts.Token); err != nil {
		return err
	}
	upstreamURL, err := url.Parse(opts.Upstream)
	if err != nil {
		return fmt.Errorf("parse upstream %q: %w", opts.Upstream, err)
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return fmt.Errorf("upstream %q must be an absolute URL (scheme://host)", opts.Upstream)
	}

	// Content-addressed recovery store (#597). Fail-open: a setup error logs
	// and runs the proxy with CCR disabled rather than aborting startup.
	var ccr *TeeStore
	if opts.CCREnabled {
		store, err := NewTeeStore()
		if err != nil {
			opts.Logger.Warn("dex proxy: CCR disabled (tee store setup failed)", "err", err)
		} else {
			ccr = store
		}
	}

	handler := newProxyHandler(upstreamURL, opts.Logger, opts.Stats, opts.Token, opts.ToolDescMode, opts.RouteConfig, opts.EditFailHook, opts.BudgetLog, opts.Pricing, opts.CostHook, opts.Effort, ccr)

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

// FetchStats fetches a Snapshot from a running proxy at addr (host:port). When
// token is non-empty it is sent in the X-Dex-Proxy-Token header so --stats can
// reach a token-gated proxy.
func FetchStats(ctx context.Context, addr, token string) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/stats", nil)
	if err != nil {
		return Snapshot{}, err
	}
	if token != "" {
		req.Header.Set(ProxyTokenHeader, token)
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
//   - POST /compact → record a PreCompact event in the budget log
//   - everything else → compress + forward to upstream
func newProxyHandler(upstream *url.URL, logger *slog.Logger, stats *Stats, token string, toolDescMode ToolDescMode, routeCfg ModelRouteConfig, editFailHook func(string), budgetLog *BudgetLog, pricing map[string]ModelPricing, costHook func(float64), effort EffortLevel, ccr *TeeStore) http.Handler {
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
			// The proxy's own auth token is for the loopback hop only; never
			// leak it upstream. The upstream Authorization / x-api-key is left
			// untouched.
			pr.Out.Header.Del(ProxyTokenHeader)
		},
		// Fail open: upstream errors must not crash the proxy.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			logger.Warn("dex proxy forward error", "method", r.Method, "path", r.URL.Path, "err", e)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	core := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /stats — JSON snapshot of cumulative token counters (no PII).
		if r.Method == http.MethodGet && r.URL.Path == "/stats" {
			snap := stats.Snapshot()
			if budgetLog != nil {
				snap.LogPath = budgetLog.LogPath()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snap)
			return
		}

		// POST /compact — fired by the PreCompact hook to record a context
		// compaction event and advance the window counter.
		if r.Method == http.MethodPost && r.URL.Path == "/compact" {
			if budgetLog != nil {
				_, _ = budgetLog.AppendCompact()
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// effectiveModel is set during POST /v1/messages processing and captured
		// by the usage tee writer closure below for per-response cost attribution.
		var effectiveModel string

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

			// Model routing runs first so that subsequent passes (pruning,
			// cache alignment) use the model-specific floors of the routed
			// model, not the originally-requested one.
			routed, routeStats := RouteModel(current, int(before), routeCfg)
			if routeStats.Applied {
				current = routed
				paths = append(paths, "route:"+routeStats.RoutedModel)
			}
			effectiveModel = routeStats.RoutedModel
			stats.recordRoute(routeStats)

			// Over-pruning signal (#561): measured on the ORIGINAL body, where
			// the client still re-sends both the old and the re-read copies of a
			// file. Pure measurement — does not alter pruning.
			reReads := AnalyzeReReadsBody(body, DefaultKeepRecent)
			stats.recordReReads(reReads)

			// Edit-fail signal (#58): detect compressed read → Edit error on the
			// same path. Measured on the original body before any rewrite.
			editFails := AnalyzeEditFailsBody(body, DefaultKeepRecent)
			stats.recordEditFails(editFails, editFailHook)

			pruned, prunedBytes, pruneSt := PruneRequestBody(current, DefaultKeepRecent, ccr)
			if prunedBytes > 0 {
				current = pruned
				paths = append(paths, "prune")
			}
			stats.recordPrune(pruneSt)

			// CCR re-injection (#597): restore any recoverable content marked in
			// the keep-recent window. No-op on v1 traffic (pruning only marks the
			// old region); positioned for the deferred active-recovery trigger.
			// GC self-throttles, so calling it per request is cheap.
			if ccr != nil {
				ccr.MaybeGC()
				// Option 2 (#640): actively collapse keep-window re-reads of
				// already-teed files to markers, then let ExpandMarkers restore
				// the exact bytes pre-send. Collapse touches only the volatile
				// tail (len-keepRecent .. end), so the cache-stable prefix that
				// AlignCacheBreakpoints marks below stays byte-identical.
				if collapsed, n := ccr.CollapseReReads(current, DefaultKeepRecent); n > 0 {
					current = collapsed
				}
				expanded, restored := ccr.ExpandMarkers(current, DefaultKeepRecent)
				if restored > 0 {
					current = expanded
					paths = append(paths, "ccr")
				}
				stats.recordCCR(restored)
			}

			// Reasoning-effort passthrough (#30): inject a budget hint after
			// routing (so the model name is final) but before cache alignment
			// (so the rewrite is included in the stable prefix).
			effortRewritten, effortStats := ApplyEffort(current, effort)
			if effortStats.Applied {
				current = effortRewritten
				paths = append(paths, "effort:"+effortStats.Effort)
			}

			// Tool-description compression runs before cache alignment so the
			// cache pass marks breakpoints on the final (compressed) tools
			// block. The mode is static per session, so the tools prefix stays
			// byte-identical turn-over-turn and keeps the cache warm.
			toolCompressed, toolDescStats := CompressToolDescriptions(current, toolDescMode)
			if toolDescStats.Applied {
				current = toolCompressed
				paths = append(paths, "tooldesc")
			}

			// Cache-breakpoint alignment runs LAST, on the post-pruned bytes
			// (see cache.go) so the marked prefix is the deterministic stable
			// region the next turn re-sends and reads from cache.
			aligned, cacheStats := AlignCacheBreakpoints(current, DefaultKeepRecent)
			if cacheStats.Applied {
				current = aligned
				paths = append(paths, "cache")
			}

			after := countBodyTokens(current)
			stats.record(before, after)
			stats.recordCache(cacheStats)
			stats.recordToolDesc(toolDescStats)
			logRequestMetrics(logger, r, current, before, after, paths, cacheStats, toolDescStats, reReads, routeStats)

			r.Body = io.NopCloser(bytes.NewReader(current))
			r.ContentLength = int64(len(current))
		}

		// Intercept the response body to extract provider-reported token counts
		// from SSE usage chunks (#57). The tee writer passes all bytes through
		// unchanged and fires notify once Done() is called.
		tw := newUsageTeeWriter(w, func(u ProviderUsage) {
			stats.recordUsage(u)
			if budgetLog != nil {
				_ = budgetLog.AppendTurn(u)
			}
			cost := ComputeCost(u, effectiveModel, pricing)
			stats.recordCost(cost)
			if costHook != nil && cost > 0 {
				costHook(cost)
			}
		})
		rp.ServeHTTP(tw, r)
		tw.Done()
	})

	// Gate every route (including /stats) behind the proxy token when set.
	return authGate(token, core)
}
