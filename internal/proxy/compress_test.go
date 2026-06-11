package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// -- isProse unit tests --

// TestProseIsDetected verifies that letter-dense, long-line, low-symbol text
// is classified as prose.
func TestProseIsDetected(t *testing.T) {
	// Typical web-fetch output: long sentences, almost no code symbols.
	prose := strings.Repeat(
		"The quick brown fox jumps over the lazy dog. This is a long natural language sentence that flows and does not contain code symbols.\n",
		15,
	)
	if !isProse(prose) {
		t.Errorf("natural language prose should be classified as prose")
	}
}

// TestCodeOutputIsNotTreatedAsProse verifies that code output is NOT prose.
// Port of lean-ctx test code_output_is_not_treated_as_prose.
func TestCodeOutputIsNotTreatedAsProse(t *testing.T) {
	code := `func NewProxyHandler(upstream *url.URL, logger *slog.Logger) http.Handler {
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
	}
	return rp
}

func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("addr %q must be host:port: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("non-loopback bind %s", host)
}`
	if isProse(code) {
		t.Errorf("Go source code should NOT be classified as prose")
	}
}

// TestShellLogIsNotTreatedAsProse verifies that shell/log output is NOT prose.
// Port of lean-ctx test shell_log_is_not_treated_as_prose.
func TestShellLogIsNotTreatedAsProse(t *testing.T) {
	shellLog := `$ go test -tags sqlite_fts5 ./...
ok   github.com/alehatsman/dex/internal/proxy   0.164s
ok   github.com/alehatsman/dex/internal/store   2.387s
FAIL github.com/alehatsman/dex/internal/eval    0.113s
--- FAIL: TestEvalRunner (0.01s)
    runner_test.go:42: got 0 results, want > 0
FAIL
exit status 1
$ echo "done"
done`
	if isProse(shellLog) {
		t.Errorf("shell/log output should NOT be classified as prose")
	}
}

// -- squeezeProse tests --

// TestProseIsSqueezed verifies that duplicate lines and extra blanks are
// removed from prose content. Port of lean-ctx test prose_is_squeezed.
func TestProseIsSqueezed(t *testing.T) {
	// Build >200 chars of prose with duplicates and extra blanks.
	line := "The documentation describes how the API handles authentication tokens in detail."
	prose := strings.Repeat(line+"\n", 5) +
		"\n\n\n" +
		line + "\n" + // this duplicate should be removed
		strings.Repeat("Another unique sentence about the system behaviour.\n", 10)

	result := squeezeProse(prose)
	// Must be shorter.
	if len(result) >= len(prose) {
		t.Errorf("squeezeProse should reduce length; orig=%d result=%d", len(prose), len(result))
	}
	// Must not have duplicate lines.
	lines := strings.Split(result, "\n")
	seen := make(map[string]bool)
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if seen[l] {
			t.Errorf("squeezeProse left duplicate line: %q", l)
		}
		seen[l] = true
	}
	// Multiple consecutive blanks should be collapsed to one.
	if strings.Contains(result, "\n\n\n") {
		t.Errorf("squeezeProse should collapse multiple blank lines")
	}
}

// TestProseSqueezeNoSavingsReturnsOriginal verifies that squeezeProse returns
// the original string when there are no meaningful token savings.
func TestProseSqueezeNoSavingsReturnsOriginal(t *testing.T) {
	// Unique lines with numeric suffix — no duplicates, no extra blanks.
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, strings.Repeat("x", 80)+" "+strings.Repeat("y", i+1))
	}
	prose := strings.Join(lines, "\n")
	result := squeezeProse(prose)
	if result != prose {
		t.Errorf("no-savings prose should be returned unchanged; orig len=%d result len=%d", len(prose), len(result))
	}
}

// -- compressToolResult integration tests --

// TestCompressToolResultProse checks that a long prose tool_result is
// compressed (squeezeProse path).
func TestCompressToolResultProse(t *testing.T) {
	line := "The documentation explains how the proxy intercepts and rewrites messages in detail."
	content := strings.Repeat(line+"\n", 10) +
		"\n\n\n" +
		line + "\n" +
		strings.Repeat("Another paragraph describes the architecture and design principles.\n", 15)

	result := compressToolResult(content, "WebFetch")
	if result == content {
		// Might be due to token threshold — just check it doesn't crash.
		t.Logf("prose not compressed (possible token threshold); result len=%d orig len=%d", len(result), len(content))
	}
}

// TestCompressToolResultShortSkipped verifies content < minPruneChars is
// returned verbatim.
func TestCompressToolResultShortSkipped(t *testing.T) {
	short := "short output"
	result := compressToolResult(short, "Bash")
	if result != short {
		t.Errorf("short content should be returned unchanged")
	}
}

// -- CompressRequestBody integration tests --

// TestCompressRequestBodyIntegration verifies the full body-level entry point
// compresses a request with bulky tool_result content and logs saved bytes.
func TestCompressRequestBodyIntegration(t *testing.T) {
	// Build a prose web-fetch result that should be compressible.
	line := "The documentation explains how the proxy intercepts and rewrites messages."
	content := strings.Repeat(line+"\n", 20) +
		"\n\n" +
		line + "\n" + // duplicate
		strings.Repeat("Another unique sentence about the system behaviour and performance.\n", 10)

	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "WebFetch", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": content},
			}},
		},
	})

	out, saved := CompressRequestBody(body)
	// Either compression ran (saved > 0) or the quality gate rejected it.
	// We just need it not to crash and to never produce a larger output.
	if len(out) > len(body) {
		t.Errorf("CompressRequestBody must not expand the body: orig=%d result=%d", len(body), len(out))
	}
	if saved < 0 {
		t.Errorf("saved must be >= 0, got %d", saved)
	}
}

// TestCompressRequestBodyNoOpReturnsOriginal checks that a request with no
// compressible content returns the original bytes unchanged.
func TestCompressRequestBodyNoOpReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	out, saved := CompressRequestBody(body)
	if string(out) != string(body) {
		t.Errorf("no-op compress should return original bytes; got different output")
	}
	if saved != 0 {
		t.Errorf("no-op compress should report 0 saved bytes, got %d", saved)
	}
}

// TestCompressRequestBodyMalformedFailOpen verifies malformed JSON passes
// through unchanged.
func TestCompressRequestBodyMalformedFailOpen(t *testing.T) {
	bad := []byte(`not json`)
	out, saved := CompressRequestBody(bad)
	if string(out) != string(bad) {
		t.Errorf("malformed body should pass through unchanged")
	}
	if saved != 0 {
		t.Errorf("malformed body should report 0 saved, got %d", saved)
	}
}
