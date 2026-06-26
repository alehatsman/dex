package proxy

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestScanSSEChunk covers all provider formats and edge cases.
func TestScanSSEChunk(t *testing.T) {
	tests := []struct {
		name string
		data string
		want ProviderUsage
	}{
		{
			name: "anthropic message_start",
			data: `data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":25,"cache_read_input_tokens":10}}}`,
			want: ProviderUsage{InputTokens: 100, CacheWriteTokens: 25, CacheReadTokens: 10},
		},
		{
			name: "anthropic message_delta",
			data: `data: {"type":"message_delta","usage":{"output_tokens":50}}`,
			want: ProviderUsage{OutputTokens: 50},
		},
		{
			name: "openai final chunk",
			data: `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":60,"prompt_tokens_details":{"cached_tokens":30}}}`,
			want: ProviderUsage{InputTokens: 120, OutputTokens: 60, CacheReadTokens: 30},
		},
		{
			name: "gemini chunk",
			data: `data: {"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"cachedContentTokenCount":20}}`,
			want: ProviderUsage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20},
		},
		{
			name: "no usage field",
			data: `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`,
			want: ProviderUsage{},
		},
		{
			name: "usage substring but unparseable JSON",
			data: `data: {"usage": not-valid-json`,
			want: ProviderUsage{},
		},
		{
			name: "empty line",
			data: ``,
			want: ProviderUsage{},
		},
		{
			name: "SSE comment line",
			data: `: keep-alive`,
			want: ProviderUsage{},
		},
		{
			name: "anthropic message_start no cache fields",
			data: `data: {"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
			want: ProviderUsage{InputTokens: 42},
		},
		{
			name: "openai no cached_tokens",
			data: `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			want: ProviderUsage{InputTokens: 10, OutputTokens: 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSSEChunk([]byte(tc.data))
			if got != tc.want {
				t.Errorf("scanSSEChunk(%q)\n got  %+v\nwant %+v", tc.data, got, tc.want)
			}
		})
	}
}

// TestUsageTeeWriterAccumulates verifies the writer accumulates usage across
// multiple Write calls and fires notify with the total sum.
func TestUsageTeeWriterAccumulates(t *testing.T) {
	rec := httptest.NewRecorder()

	var notified ProviderUsage
	tw := newUsageTeeWriter(rec, func(u ProviderUsage) {
		notified = u
	})

	chunks := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":25,\"cache_read_input_tokens\":10}}}\n",
		"\n",
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n",
		"\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n",
	}

	var want strings.Builder
	for _, c := range chunks {
		n, err := tw.Write([]byte(c))
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if n != len(c) {
			t.Fatalf("Write returned %d, want %d", n, len(c))
		}
		want.WriteString(c)
	}

	tw.Done()

	expected := ProviderUsage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheWriteTokens: 25,
		CacheReadTokens:  10,
	}
	if notified != expected {
		t.Errorf("notify got %+v, want %+v", notified, expected)
	}

	// Verify passthrough: all bytes forwarded unchanged.
	if rec.Body.String() != want.String() {
		t.Errorf("body passthrough mismatch:\n got  %q\nwant %q", rec.Body.String(), want.String())
	}
}

// TestUsageTeeWriterSplitLine verifies a line split across two Write calls is
// handled correctly (carry-over buffer).
func TestUsageTeeWriterSplitLine(t *testing.T) {
	rec := httptest.NewRecorder()

	var notified ProviderUsage
	tw := newUsageTeeWriter(rec, func(u ProviderUsage) {
		notified = u
	})

	line := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":77}}}\n"
	half := len(line) / 2

	// Write in two halves without the newline in the first half.
	if _, err := tw.Write([]byte(line[:half])); err != nil {
		t.Fatalf("Write(first half): %v", err)
	}
	if _, err := tw.Write([]byte(line[half:])); err != nil {
		t.Fatalf("Write(second half): %v", err)
	}
	tw.Done()

	if notified.InputTokens != 77 {
		t.Errorf("input_tokens = %d, want 77", notified.InputTokens)
	}

	// Verify all bytes forwarded.
	if rec.Body.String() != line {
		t.Errorf("body passthrough mismatch: got %q, want %q", rec.Body.String(), line)
	}
}

// TestUsageTeeWriterFlush verifies Flush() is forwarded to the underlying
// Flusher — critical for SSE streaming to work.
func TestUsageTeeWriterFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newUsageTeeWriter(rec, func(ProviderUsage) {})
	// httptest.ResponseRecorder implements http.Flusher.
	// Calling Flush must not panic.
	tw.Flush()
	if !rec.Flushed {
		t.Error("Flush() did not flush the underlying ResponseRecorder")
	}
}

// TestStatsEndpointUsageTokens verifies the proxy /stats endpoint returns
// non-zero input_tokens / output_tokens when the upstream serves an SSE
// response containing Anthropic usage chunks.
func TestStatsEndpointUsageTokens(t *testing.T) {
	sseBody := strings.Join([]string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"cache_creation_input_tokens\":30,\"cache_read_input_tokens\":5}}}\n",
		"\n",
		"data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n",
		"\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":80}}\n",
		"\n",
		"data: [DONE]\n",
	}, "")

	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	})

	front, _ := newTestProxy(t, up)

	reqBody := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	statsResp, err := http.Get(front.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer statsResp.Body.Close()

	var snap Snapshot
	if err := json.NewDecoder(statsResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}

	if snap.InputTokens != 200 {
		t.Errorf("input_tokens = %d, want 200", snap.InputTokens)
	}
	if snap.OutputTokens != 80 {
		t.Errorf("output_tokens = %d, want 80", snap.OutputTokens)
	}
	if snap.CacheWriteTokens != 30 {
		t.Errorf("cache_write_tokens = %d, want 30", snap.CacheWriteTokens)
	}
	if snap.CacheReadTokens != 5 {
		t.Errorf("cache_read_tokens = %d, want 5", snap.CacheReadTokens)
	}
}

// TestStatsEndpointUsageTokensGzipSSE is the production-failure regression test
// (#695). Anthropic returns Content-Encoding: gzip when the request includes
// Accept-Encoding. The proxy must strip Accept-Encoding from the outgoing
// request so Go's transport auto-decompresses the response before the tee
// writer scans it — otherwise the tee writer sees gzip bytes and records 0
// for every usage field.
func TestStatsEndpointUsageTokensGzipSSE(t *testing.T) {
	sseBody := strings.Join([]string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":150,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":20}}}\n",
		"\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n",
		"\n",
		"data: [DONE]\n",
	}, "")

	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The proxy strips the client's Accept-Encoding header so Go's
		// transport can add its own "gzip" and auto-decompress. The upstream
		// should therefore NOT see the original multi-value client header.
		if ae := r.Header.Get("Accept-Encoding"); ae == "gzip, deflate, br" {
			t.Errorf("proxy forwarded client Accept-Encoding verbatim (%q); "+
				"want it stripped so transport handles decompression", ae)
		}
		_, _ = io.Copy(io.Discard, r.Body)

		// Compress the SSE body with gzip, mimicking what Anthropic does when
		// Accept-Encoding: gzip was present in the original client request.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, sseBody)
		_ = gz.Close()
	})

	front, _ := newTestProxy(t, up)

	// Send a request with Accept-Encoding — exactly as Claude Code (Node.js)
	// does. The proxy must strip it before forwarding to the upstream.
	reqBody := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages",
		strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	statsResp, err := http.Get(front.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer statsResp.Body.Close()

	var snap Snapshot
	if err := json.NewDecoder(statsResp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}

	if snap.InputTokens != 150 {
		t.Errorf("input_tokens = %d, want 150", snap.InputTokens)
	}
	if snap.OutputTokens != 40 {
		t.Errorf("output_tokens = %d, want 40", snap.OutputTokens)
	}
	if snap.CacheReadTokens != 20 {
		t.Errorf("cache_read_tokens = %d, want 20", snap.CacheReadTokens)
	}
}
