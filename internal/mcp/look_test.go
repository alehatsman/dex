package mcp

import (
	"context"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestClassifyLookTarget is the disambiguation matrix for the look router — the
// crux of the verb. Every row pins a target to the lane it must route to (and the
// cleaned argument that lane receives). The one hard case is separating a
// package-tail symbol (mcp.NewServer) from a file path (config.yaml): both have a
// dot, and only the extension allowlist tells them apart.
func TestClassifyLookTarget(t *testing.T) {
	cases := []struct {
		target      string
		wantKind    string
		wantCleaned string
	}{
		// paths — slash or known extension.
		{"internal/mcp/server.go", "read", "internal/mcp/server.go"},
		{"server.go", "read", "server.go"},
		{"README.md", "read", "README.md"},
		{"config.yaml", "read", "config.yaml"},
		{".gitignore", "read", ".gitignore"},
		{"./Makefile.go", "read", "./Makefile.go"},
		{"a/b/c", "read", "a/b/c"}, // slash wins even without an extension
		{"~/notes.md", "read", "~/notes.md"},

		// locations — path-like head + trailing :line[:col].
		{"server.go:829", "locate", "server.go:829"},
		{"internal/mcp/server.go:829:12", "locate", "internal/mcp/server.go:829:12"},
		{"a/b.go:1", "locate", "a/b.go:1"},

		// regex — explicit /.../ delimiters win over everything.
		{"/func .*Verb/", "grep", "func .*Verb"},
		{"/a/b/", "grep", "a/b"},           // slashes inside are content, not a path
		{"/server.go:1/", "grep", "server.go:1"},

		// symbols — no path signal; the trace default.
		{"NewServer", "trace", "NewServer"},
		{"handleRequest", "trace", "handleRequest"},
		{"(*Server).Run", "trace", "(*Server).Run"}, // dot present, ext "Run" not in allowlist
		{"mcp.NewServer", "trace", "mcp.NewServer"},  // package-tail, NOT a path
		{"Foo:12", "trace", "Foo:12"},                // bare-symbol head → not a location

		// whitespace is trimmed before classification.
		{"  NewServer  ", "trace", "NewServer"},
	}
	for _, c := range cases {
		t.Run(c.target, func(t *testing.T) {
			kind, cleaned := classifyLookTarget(c.target)
			if kind != c.wantKind {
				t.Errorf("classifyLookTarget(%q) kind = %q, want %q", c.target, kind, c.wantKind)
			}
			if cleaned != c.wantCleaned {
				t.Errorf("classifyLookTarget(%q) cleaned = %q, want %q", c.target, cleaned, c.wantCleaned)
			}
		})
	}
}

func TestClassifyLookTargetEmpty(t *testing.T) {
	if kind, _ := classifyLookTarget("   "); kind != "" {
		t.Errorf("empty target should not classify, got %q", kind)
	}
}

// TestLookVerbEmptyTargetErrors: no target is a caller error, not a lane call.
func TestLookVerbEmptyTargetErrors(t *testing.T) {
	h := stubServer(t)
	_, out, err := lookVerb(context.Background(), h, &sdk.CallToolRequest{}, LookInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "error" || out.Trust.Provenance != "exact" {
		t.Fatalf("want error/exact envelope, got status=%q trust=%q", out.Status, out.Trust.Provenance)
	}
}

// TestLookVerbUnknownKindErrors: an explicit bogus kind is rejected, not routed.
func TestLookVerbUnknownKindErrors(t *testing.T) {
	h := stubServer(t)
	_, out, err := lookVerb(context.Background(), h, &sdk.CallToolRequest{}, LookInput{Target: "x", Kind: "sniff"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "error" {
		t.Fatalf("want error for unknown kind, got %q", out.Status)
	}
}

// TestLookVerbGrepRoutesAndChains: a /regex/ target routes to the grep lane and,
// on a hit, the envelope carries a next step to read the first match. Uses a real
// index over a temp project so the grep lane actually matches.
func TestLookVerbGrepRoutesAndChains(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\n\nfunc lookNeedle() int { return 42 }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	h := newServer(srv.URL, cacheDir)

	_, out, err := lookVerb(context.Background(), h, &sdk.CallToolRequest{}, LookInput{Target: "/lookNeedle/", ProjectRoot: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Result.Kind != "grep" {
		t.Fatalf("want grep lane, got %q", out.Result.Kind)
	}
	if out.Trust.Provenance != "exact" {
		t.Fatalf("want exact provenance, got %q", out.Trust.Provenance)
	}
	if out.Result.Grep == nil || len(out.Result.Grep.Matches) == 0 {
		t.Fatalf("want at least one grep match, got %+v", out.Result.Grep)
	}
	if len(out.Next) == 0 || out.Next[0].Verb != "look" {
		t.Fatalf("want a look next-step after grep, got %+v", out.Next)
	}
}
