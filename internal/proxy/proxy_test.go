package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	front := httptest.NewServer(newProxyHandler(upURL, logger))
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
	if !strings.Contains(out, "dex proxy request") || !strings.Contains(out, "input_tokens=") {
		t.Errorf("expected input-token baseline log, got:\n%s", out)
	}
	if !strings.Contains(out, "tokenizer=cl100k_base") {
		t.Errorf("claude model should select cl100k tokenizer, got:\n%s", out)
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
	if !strings.Contains(logs.String(), "raw_body_fallback") {
		t.Errorf("expected raw_body_fallback baseline log, got:\n%s", logs.String())
	}
}

// TestFailOpenUpstreamDown asserts that when the upstream is unreachable the
// proxy returns a 502 (never a panic / hang) — the ErrorHandler path.
func TestFailOpenUpstreamDown(t *testing.T) {
	// Point at a closed port so the dial fails immediately.
	upURL, _ := url.Parse("http://127.0.0.1:1")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	front := httptest.NewServer(newProxyHandler(upURL, logger))
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

func TestValidateLoopback(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:8788", false},
		{"localhost:8788", false},
		{"[::1]:8788", false},
		{"127.0.0.2:8788", false}, // 127.0.0.0/8 is all loopback
		{":8788", true},           // all interfaces
		{"0.0.0.0:8788", true},
		{"192.168.1.10:8788", true},
		{"example.com:8788", true},
		{"127.0.0.1", true}, // missing port
	}
	for _, c := range cases {
		err := validateLoopback(c.addr)
		if (err != nil) != c.wantErr {
			t.Errorf("validateLoopback(%q) err=%v, wantErr=%v", c.addr, err, c.wantErr)
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

	// Build a request with 12 messages: assistant tool_use + user tool_result
	// (Read, bulky) + 10 padding user messages so the result is outside
	// keep_recent=10.
	bigContent := strings.Repeat("line content here\n", 20) // > 200 chars
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "tid1", "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tid1", "content": bigContent},
			}},
			map[string]any{"role": "user", "content": "pad1"},
			map[string]any{"role": "user", "content": "pad2"},
			map[string]any{"role": "user", "content": "pad3"},
			map[string]any{"role": "user", "content": "pad4"},
			map[string]any{"role": "user", "content": "pad5"},
			map[string]any{"role": "user", "content": "pad6"},
			map[string]any{"role": "user", "content": "pad7"},
			map[string]any{"role": "user", "content": "pad8"},
			map[string]any{"role": "user", "content": "pad9"},
			map[string]any{"role": "user", "content": "pad10"},
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

	// The pruned body forwarded upstream must be smaller.
	if len(upstreamBody) >= len(body) {
		t.Errorf("upstream body (%d bytes) not smaller than original (%d bytes)", len(upstreamBody), len(body))
	}
	// Upstream body must not contain the original file content.
	if strings.Contains(upstreamBody, "line content here") {
		t.Errorf("original file content leaked to upstream; should have been pruned")
	}
	// Log must contain saved_bytes.
	if !strings.Contains(logs.String(), "saved_bytes") {
		t.Errorf("expected saved_bytes log; got:\n%s", logs.String())
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
