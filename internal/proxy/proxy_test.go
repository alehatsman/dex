package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestProxy spins up the proxy handler in front of a caller-supplied
// upstream and returns a client-facing httptest server plus a captured-log
// buffer. The handler is the production newProxyHandler, so these exercise the
// real forward path.
func newTestProxy(t *testing.T, upstream http.Handler) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	front := httptest.NewServer(newProxyHandler(upURL, logger, &Stats{}, "", ToolDescFull, ModelRouteConfig{}))
	t.Cleanup(front.Close)
	return front, &logs
}

// TestForwardVerbatim asserts the proxy forwards method, path, headers, and
// body to upstream unchanged, and returns the upstream status + body.
func TestForwardVerbatim(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	front, _ := newTestProxy(t, up)

	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Errorf("upstream saw %s %s, want POST /v1/messages", gotMethod, gotPath)
	}
	if gotAuth != "sk-secret" {
		t.Errorf("api key not forwarded: got %q", gotAuth)
	}
	if gotBody != body {
		t.Errorf("body altered:\n got %q\nwant %q", gotBody, body)
	}
	if resp.Header.Get("X-Upstream") != "yes" {
		t.Errorf("upstream response header not passed through")
	}
	rb, _ := io.ReadAll(resp.Body)
	if string(rb) != `{"ok":true}` {
		t.Errorf("response body altered: %q", rb)
	}
}

// TestSSEStreamingPassthrough verifies the proxy flushes SSE chunks as they
// arrive rather than buffering the whole stream — the agent must see events
// incrementally. The upstream emits two events with a gap; the client must
// observe the first before the second is sent.
func TestSSEStreamingPassthrough(t *testing.T) {
	release := make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"a\":1}\n\n")
		fl.Flush()
		<-release // hold the second event until the client has read the first
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"b\":2}\n\n")
		fl.Flush()
	})
	front, _ := newTestProxy(t, up)

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)
	// First event must arrive before we release the second — proves no buffering.
	first, err := readEvent(br)
	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if !strings.Contains(first, "message_start") {
		t.Errorf("first event = %q, want message_start", first)
	}
	close(release)
	second, err := readEvent(br)
	if err != nil {
		t.Fatalf("read second event: %v", err)
	}
	if !strings.Contains(second, "message_stop") {
		t.Errorf("second event = %q, want message_stop", second)
	}
}

// readEvent reads one SSE event (up to a blank-line terminator) with a guard
// timeout so a buffering regression fails fast instead of hanging the suite.
func readEvent(br *bufio.Reader) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			b.WriteString(line)
			if err != nil {
				ch <- res{b.String(), err}
				return
			}
			if line == "\n" {
				ch <- res{b.String(), nil}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(2 * time.Second):
		return "", context.DeadlineExceeded
	}
}

// TestTokenBaselineLogged checks the per-request input-token count is logged
// for a well-formed /v1/messages body.
func TestTokenBaselineLogged(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain so the proxy's tee'd body is exercised end-to-end.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	front, logs := newTestProxy(t, up)

	body := `{"model":"claude-sonnet-4-6","system":"You are helpful.","messages":[{"role":"user","content":[{"type":"text","text":"summarize this repository in detail"}]}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	out := logs.String()
	if !strings.Contains(out, "dex proxy request") || !strings.Contains(out, "tokens_before=") {
		t.Errorf("expected per-request metrics log, got:\n%s", out)
	}
	if !strings.Contains(out, "tokens_after=") {
		t.Errorf("expected tokens_after in log, got:\n%s", out)
	}
}

// TestFailOpenMalformedBody asserts a body that isn't valid JSON still
// forwards verbatim (the token baseline falls back, never blocks the request).
func TestFailOpenMalformedBody(t *testing.T) {
	var gotBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	front, logs := newTestProxy(t, up)

	body := `this is not json {{{`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("malformed body should still forward (200), got %d", resp.StatusCode)
	}
	if gotBody != body {
		t.Errorf("malformed body not forwarded verbatim: got %q", gotBody)
	}
	// Malformed body still gets a per-request metrics log; pass=passthrough
	// since prune fails open on non-JSON.
	if !strings.Contains(logs.String(), "dex proxy request") {
		t.Errorf("expected per-request metrics log even for malformed body, got:\n%s", logs.String())
	}
}

// TestFailOpenUpstreamDown asserts that when the upstream is unreachable the
// proxy returns a 502 (never a panic / hang) — the ErrorHandler path.
func TestFailOpenUpstreamDown(t *testing.T) {
	// Hold a listener that immediately closes every connection. Holding the
	// port avoids the ephemeral-port-reuse race (another parallel test
	// process grabbing a just-closed port and answering 200); the reset
	// forces the proxy round trip to fail -> ErrorHandler -> 502.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	upURL, _ := url.Parse("http://" + addr)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	front := httptest.NewServer(newProxyHandler(upURL, logger, &Stats{}, "", ToolDescFull, ModelRouteConfig{}))
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("upstream down should yield 502, got %d", resp.StatusCode)
	}
}

func TestValidateBind(t *testing.T) {
	cases := []struct {
		addr    string
		token   string
		wantErr bool
	}{
		// Loopback always allowed, token or not.
		{"127.0.0.1:8788", "", false},
		{"localhost:8788", "", false},
		{"[::1]:8788", "", false},
		{"127.0.0.2:8788", "", false}, // 127.0.0.0/8 is all loopback
		// Non-loopback refused without a token...
		{":8788", "", true}, // all interfaces
		{"0.0.0.0:8788", "", true},
		{"192.168.1.10:8788", "", true},
		{"example.com:8788", "", true},
		// ...but allowed once DEX_PROXY_TOKEN is set (conscious opt-in).
		{"0.0.0.0:8788", "tok", false},
		{"192.168.1.10:8788", "tok", false},
		// Malformed addr fails regardless of token.
		{"127.0.0.1", "", true}, // missing port
		{"127.0.0.1", "tok", true},
	}
	for _, c := range cases {
		err := validateBind(c.addr, c.token)
		if (err != nil) != c.wantErr {
			t.Errorf("validateBind(%q, token=%q) err=%v, wantErr=%v", c.addr, c.token, err, c.wantErr)
		}
	}
}

// TestAuthGate verifies that when a token is set, incoming requests must carry
// it in X-Dex-Proxy-Token; the gate header is stripped before forwarding; and
// the upstream credential (x-api-key) is passed through untouched.
func TestAuthGate(t *testing.T) {
	const token = "proxytok-secret"
	var gotProxyTok, gotAPIKey string
	var reached bool
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		gotProxyTok = r.Header.Get(ProxyTokenHeader)
		gotAPIKey = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	})
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	upURL, _ := url.Parse(upSrv.URL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	front := httptest.NewServer(newProxyHandler(upURL, logger, &Stats{}, token, ToolDescFull, ModelRouteConfig{}))
	defer front.Close()

	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`

	// 1. No token → 401, upstream never reached.
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", resp.StatusCode)
	}
	if reached {
		t.Errorf("upstream reached despite missing proxy token")
	}

	// 2. Wrong token → 401.
	reached = false
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set(ProxyTokenHeader, "wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", resp.StatusCode)
	}
	if reached {
		t.Errorf("upstream reached despite wrong proxy token")
	}

	// 3. Correct token → forwarded; gate header stripped; x-api-key intact.
	reached = false
	req, _ = http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set(ProxyTokenHeader, token)
	req.Header.Set("x-api-key", "sk-upstream-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200", resp.StatusCode)
	}
	if !reached {
		t.Errorf("upstream not reached with correct proxy token")
	}
	if gotProxyTok != "" {
		t.Errorf("proxy token leaked upstream: %q (must be stripped)", gotProxyTok)
	}
	if gotAPIKey != "sk-upstream-secret" {
		t.Errorf("upstream credential not passed through: %q", gotAPIKey)
	}

	// 4. /stats is gated too.
	resp, err = http.Get(front.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/stats without token: status = %d, want 401", resp.StatusCode)
	}
}

// TestNoSecretsInLogs is the #240 acceptance check: after a request carrying an
// API key and conversation content, the proxy logs must contain no occurrence
// of the key, a Bearer token, or message content — only counts/ratios/paths.
func TestNoSecretsInLogs(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	front, logs := newTestProxy(t, up)

	const secretSentence = "SUPERSECRETCONVERSATIONCONTENT"
	body := `{"model":"claude-sonnet-4-6","system":"You are helpful.","messages":[{"role":"user","content":[{"type":"text","text":"` + secretSentence + `"}]}]}`
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-ant-verysecretkey123")
	req.Header.Set("Authorization", "Bearer sk-ant-bearersecret456")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	out := logs.String()
	for _, leak := range []string{"sk-ant-", "Bearer", secretSentence, "verysecretkey", "bearersecret"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q:\n%s", leak, out)
		}
	}
}

// TestPruneIntegration verifies that the proxy rewrites bulky old tool_result
// history before forwarding and emits a "saved_bytes" log line.
func TestPruneIntegration(t *testing.T) {
	var upstreamBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})
	front, logs := newTestProxy(t, up)

	// Build a request with 26 messages: assistant tool_use + user tool_result
	// (Read, bulky) + 24 padding user messages. With PruneStride=16 and
	// keepRecent=10: pruneStart(26,10)=(16/16)*16=16, so messages 0-15
	// (including the tool_result at index 1) are in the prune zone.
	bigContent := strings.Repeat("line content here\n", 20) // > 200 chars
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tid1", "name": "Read", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tid1", "content": bigContent},
		}},
	}
	for i := 0; i < 24; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "pad"})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": msgs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	// The pruned body forwarded upstream must be smaller.
	if len(upstreamBody) >= len(body) {
		t.Errorf("upstream body (%d bytes) not smaller than original (%d bytes)", len(upstreamBody), len(body))
	}
	// Upstream body must not contain the original file content.
	if strings.Contains(upstreamBody, "line content here") {
		t.Errorf("original file content leaked to upstream; should have been pruned")
	}
	// Log must show the prune pass fired and tokens were saved.
	logOut := logs.String()
	if !strings.Contains(logOut, `pass=prune`) {
		t.Errorf("expected pass=prune in log; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "tokens_saved=") {
		t.Errorf("expected tokens_saved in log; got:\n%s", logOut)
	}
}

// TestRecentToolResultUntouched verifies that a tool_result inside the recent
// window is forwarded verbatim — its content must not be pruned or altered.
func TestRecentToolResultUntouched(t *testing.T) {
	var upstreamBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})
	front, _ := newTestProxy(t, up)

	// A single tool_use + tool_result pair — well within keep_recent=10.
	bigContent := strings.Repeat("unique marker line\n", 30) // > 200 chars, distinct
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tid1", "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tid1", "content": bigContent},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	// Content must be present verbatim in the forwarded request.
	if !strings.Contains(upstreamBody, "unique marker line") {
		t.Errorf("recent tool_result content was pruned; upstream body:\n%s", upstreamBody)
	}
}

// TestCacheBreakpointIntegration drives a large multi-turn request through the
// full forward path and asserts the proxy (a) injects cache_control breakpoints
// on the stable prefix it forwards upstream, (b) caps them at maxBreakpoints,
// and (c) logs the cache pass with a breakpoint count.
func TestCacheBreakpointIntegration(t *testing.T) {
	var upstreamBody string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		upstreamBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})
	front, logs := newTestProxy(t, up)

	// Large, stable system prompt + a long conversation so multiple stable
	// boundaries clear the cacheable floor (Sonnet 4.5 → 1024 tokens).
	bigSystem := strings.Repeat("alpha bravo charlie delta echo foxtrot ", 700)
	var msgs []any
	for i := 0; i < 24; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"role":    role,
			"content": []any{map[string]any{"type": "text", "text": strings.Repeat("lorem ipsum dolor sit amet ", 120)}},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   bigSystem,
		"messages": msgs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if !strings.Contains(upstreamBody, `"cache_control"`) {
		t.Fatalf("expected cache_control markers in upstream body")
	}
	if !strings.Contains(upstreamBody, `"ephemeral"`) {
		t.Errorf("cache_control should be type ephemeral")
	}
	// Count markers reaching upstream — must respect the max.
	got := strings.Count(upstreamBody, `"cache_control"`)
	if got == 0 || got > maxBreakpoints {
		t.Errorf("upstream has %d breakpoints, want 1..%d", got, maxBreakpoints)
	}

	logOut := logs.String()
	if !strings.Contains(logOut, "cache_breakpoints=") {
		t.Errorf("expected cache_breakpoints in log; got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "cache") { // pass label includes "cache"
		t.Errorf("expected cache pass label in log; got:\n%s", logOut)
	}
}

// TestExtractTextToolResult ensures nested tool_result content contributes to
// the token baseline — the dominant sink the follow-up tickets compress.
func TestExtractTextToolResult(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","content":[{"type":"text","text":"line1\nline2"}]}]`)
	var b strings.Builder
	extractText(&b, raw)
	if !strings.Contains(b.String(), "line1") || !strings.Contains(b.String(), "line2") {
		t.Errorf("tool_result text not extracted: %q", b.String())
	}
}

// TestStatsEndpoint verifies GET /stats returns a valid JSON Snapshot and that
// counters reflect the requests processed by the proxy.
func TestStatsEndpoint(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	front, _ := newTestProxy(t, up)

	// Send two /v1/messages requests.
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`
	for range 2 {
		resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
	}

	// Fetch stats.
	resp, err := http.Get(front.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/stats returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}
	if snap.RequestsTotal != 2 {
		t.Errorf("requests_total = %d, want 2", snap.RequestsTotal)
	}
	if snap.TokensBefore == 0 {
		t.Errorf("tokens_before = 0, expected positive")
	}
	if snap.TokensAfter == 0 {
		t.Errorf("tokens_after = 0, expected positive")
	}
	if snap.TokensSaved < 0 {
		t.Errorf("tokens_saved = %d, expected >= 0", snap.TokensSaved)
	}
}

// TestStatsCounters verifies Stats.record and Snapshot arithmetic.
func TestStatsCounters(t *testing.T) {
	s := &Stats{}
	s.record(100, 80)
	s.record(200, 200) // no compression

	snap := s.Snapshot()
	if snap.RequestsTotal != 2 {
		t.Errorf("requests_total = %d, want 2", snap.RequestsTotal)
	}
	if snap.RequestsCompressed != 1 {
		t.Errorf("requests_compressed = %d, want 1", snap.RequestsCompressed)
	}
	if snap.TokensBefore != 300 {
		t.Errorf("tokens_before = %d, want 300", snap.TokensBefore)
	}
	if snap.TokensAfter != 280 {
		t.Errorf("tokens_after = %d, want 280", snap.TokensAfter)
	}
	if snap.TokensSaved != 20 {
		t.Errorf("tokens_saved = %d, want 20", snap.TokensSaved)
	}
	wantRatio := 20.0 / 300.0
	if snap.CompressionRatio < wantRatio-0.001 || snap.CompressionRatio > wantRatio+0.001 {
		t.Errorf("compression_ratio = %f, want ~%f", snap.CompressionRatio, wantRatio)
	}
}
