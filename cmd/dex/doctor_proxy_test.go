package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TestCheckProxyUnset: no ANTHROPIC_BASE_URL → skip with the opt-in hint.
func TestCheckProxyUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	c := checkProxy(context.Background())
	if c.status != docSkip {
		t.Fatalf("status = %v, want docSkip", c.status)
	}
	if !strings.Contains(c.detail, "not set") {
		t.Errorf("detail = %q, want mention of unset", c.detail)
	}
}

// TestCheckProxyMalformed: a non-URL value → warn, no dial attempted.
func TestCheckProxyMalformed(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "::not a url::")
	c := checkProxy(context.Background())
	if c.status != docWarn {
		t.Fatalf("status = %v, want docWarn", c.status)
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
	if c.status != docWarn {
		t.Fatalf("status = %v, want docWarn", c.status)
	}
	if !strings.Contains(c.detail, "UNREACHABLE") {
		t.Errorf("detail = %q, want UNREACHABLE", c.detail)
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
	if c.status != docOK {
		t.Fatalf("status = %v, want docOK", c.status)
	}
	if !strings.Contains(c.detail, "reachable") {
		t.Errorf("detail = %q, want reachable", c.detail)
	}
	foundHint := false
	for _, h := range c.hints {
		if strings.Contains(h, "ENABLE_TOOL_SEARCH") {
			foundHint = true
		}
	}
	if !foundHint {
		t.Errorf("expected ENABLE_TOOL_SEARCH hint for non-first-party host, got %v", c.hints)
	}
}
