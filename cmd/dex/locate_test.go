package main

import "testing"

// TestParseClaimFlags covers the CLI --claim splitting: 'file:line' stays a
// bare ref, 'file:line=symbol' splits on the first '=' into ref + symbol (#708).
func TestParseClaimFlags(t *testing.T) {
	got := parseClaimFlags([]string{
		"internal/mcp/server.go:412=handleAsk",
		"internal/retrieve/fuse.go:88",
		"a.go:1=2", // first '=' wins; symbol can be anything non-empty
	})
	want := []struct{ ref, sym string }{
		{"internal/mcp/server.go:412", "handleAsk"},
		{"internal/retrieve/fuse.go:88", ""},
		{"a.go:1", "2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d claims, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Ref != w.ref || got[i].Symbol != w.sym {
			t.Errorf("claim[%d] = {%q,%q}, want {%q,%q}", i, got[i].Ref, got[i].Symbol, w.ref, w.sym)
		}
	}
}
