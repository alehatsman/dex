package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/health"
)

// TestCheckProxyUnset: no ANTHROPIC_BASE_URL → skip with the opt-in hint.
func TestCheckProxyUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	c := checkProxy(context.Background())
	if c.Status != health.Skip {
		t.Fatalf("status = %v, want Skip", c.Status)
	}
	if !strings.Contains(c.Detail, "not set") {
		t.Errorf("detail = %q, want mention of unset", c.Detail)
	}
}

// TestCheckProxyMalformed: a non-URL value → warn, no dial attempted.
func TestCheckProxyMalformed(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "::not a url::")
	c := checkProxy(context.Background())
	if c.Status != health.Warn {
		t.Fatalf("status = %v, want Warn", c.Status)
	}
}

// TestCheckProxyUnreachable: a syntactically valid URL pointing at a closed
// loopback port → warn UNREACHABLE.
func TestCheckProxyUnreachable(t *testing.T) {
	// Bind then close to obtain a port nothing is listening on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	t.Setenv("ANTHROPIC_BASE_URL", "http://"+addr)
	c := checkProxy(context.Background())
	if c.Status != health.Warn {
		t.Fatalf("status = %v, want Warn", c.Status)
	}
	if !strings.Contains(c.Detail, "UNREACHABLE") {
		t.Errorf("detail = %q, want UNREACHABLE", c.Detail)
	}
}

// TestCheckProxyReachable: a live listener → OK (reachable). A non-first-party
// host with ENABLE_TOOL_SEARCH unset gets the tool-search hint.
func TestCheckProxyReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	t.Setenv("ANTHROPIC_BASE_URL", "http://"+l.Addr().String())
	t.Setenv("ENABLE_TOOL_SEARCH", "")
	c := checkProxy(context.Background())
	if c.Status != health.OK {
		t.Fatalf("status = %v, want OK", c.Status)
	}
	if !strings.Contains(c.Detail, "reachable") {
		t.Errorf("detail = %q, want reachable", c.Detail)
	}
	foundHint := false
	for _, h := range c.Hints {
		if strings.Contains(h, "ENABLE_TOOL_SEARCH") {
			foundHint = true
		}
	}
	if !foundHint {
		t.Errorf("expected ENABLE_TOOL_SEARCH hint for non-first-party host, got %v", c.Hints)
	}
}
