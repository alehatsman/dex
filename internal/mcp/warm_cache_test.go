package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/proj"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestWarmCacheEndToEnd drives the real look→summarize path twice on one file
// in signatures mode: the first agent renders + pushes, the second gets a warm
// hit (marked in the read hint) instead of re-rendering.
func TestWarmCacheEndToEnd(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/go.mod", "module warmdemo\n\ngo 1.22\n")
	writeFile(t, projDir+"/main.go", "package main\n\n// F greets.\nfunc F() string { return \"hi\" }\n\n// G adds.\nfunc G(a, b int) int { return a + b }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)

	read := func(agent string) SummarizeOutput {
		s := newServer(srv.URL, cacheDir)
		s.AgentID = agent
		_, out, err := lookVerb(context.Background(), s, &sdk.CallToolRequest{},
			LookInput{Target: "main.go", Kind: "read", Mode: "aggressive", ProjectRoot: root})
		if err != nil {
			t.Fatalf("look(%s): %v", agent, err)
		}
		if out.Result.Read == nil {
			t.Fatalf("look(%s): no read result (status=%s)", agent, out.Status)
		}
		return *out.Result.Read
	}

	first := read("alice") // renders + pushes
	if strings.Contains(first.Hint, "warm-cache hit") {
		t.Fatalf("first read should be a cold render, got warm hint: %q", first.Hint)
	}
	second := read("bob") // should reuse alice's render
	if !strings.Contains(second.Hint, "warm-cache hit") {
		t.Errorf("second read did not hit the warm cache: hint=%q", second.Hint)
	}
	if second.Content != first.Content {
		t.Errorf("warm hit content diverged:\n first=%q\n second=%q", first.Content, second.Content)
	}
}

func TestWorthWarmCaching(t *testing.T) {
	cases := map[ReadMode]bool{
		ReadModeSignatures: true,
		ReadModeSkeleton:   true,
		ReadModeMap:        true,
		ReadModeAggressive: true,
		ReadModeFull:       false, // cheap raw slice — not worth sharing
		ReadModeLines:      false, // cheap raw slice
		ReadModeSummary:    false, // LLM, non-deterministic
	}
	for mode, want := range cases {
		if got := worthWarmCaching(mode); got != want {
			t.Errorf("worthWarmCaching(%s) = %v, want %v", mode, got, want)
		}
	}
}

// TestWarmCacheRoundTrip: a pushed compressed render is pulled back by a peer
// with the same etag (tagged as a warm hit), and a changed etag misses.
func TestWarmCacheRoundTrip(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\nfunc F() {}\n")
	root, _ := openIndexedStore(t, srv.URL, cacheDir, projDir)
	p, err := proj.Resolve(root, cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	producer := newServer(srv.URL, cacheDir)
	producer.AgentID = "alice"
	rendered := SummarizeOutput{
		Status:  "ok",
		Content: "func F()",
		Bytes:   8,
		Hint:    "signatures of main.go",
	}
	producer.warmCachePush(context.Background(), p, "main.go", ReadModeSignatures, "etag-v1", rendered)

	// A peer pulls the same (path, mode, etag) → hit, content verbatim + tag.
	peer := newServer(srv.URL, cacheDir)
	peer.AgentID = "bob"
	got, ok := peer.warmCachePull(context.Background(), p, "main.go", ReadModeSignatures, "etag-v1")
	if !ok {
		t.Fatal("expected warm-cache hit")
	}
	if got.Content != "func F()" {
		t.Errorf("content not preserved: %q", got.Content)
	}
	if !strings.Contains(got.Hint, "warm-cache hit") {
		t.Errorf("warm hint missing: %q", got.Hint)
	}

	// A changed file (new etag) must miss — never serve stale.
	if _, ok := peer.warmCachePull(context.Background(), p, "main.go", ReadModeSignatures, "etag-v2"); ok {
		t.Error("stale etag served a warm hit")
	}
	// A different mode must miss (no cross-mode collision).
	if _, ok := peer.warmCachePull(context.Background(), p, "main.go", ReadModeMap, "etag-v1"); ok {
		t.Error("cross-mode collision")
	}
}

// TestWarmCacheKillSwitch: DEX_SWARM_WARMCACHE=off disables both ends.
func TestWarmCacheKillSwitch(t *testing.T) {
	t.Setenv("DEX_SWARM_WARMCACHE", "off")
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n")
	root, _ := openIndexedStore(t, srv.URL, cacheDir, projDir)
	p, _ := proj.Resolve(root, cacheDir)

	s := newServer(srv.URL, cacheDir)
	s.warmCachePush(context.Background(), p, "main.go", ReadModeSignatures, "e1",
		SummarizeOutput{Status: "ok", Content: "x"})
	if _, ok := s.warmCachePull(context.Background(), p, "main.go", ReadModeSignatures, "e1"); ok {
		t.Error("kill switch did not disable the warm cache")
	}
}
