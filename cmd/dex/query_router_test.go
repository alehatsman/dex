package main

import (
	"context"
	"testing"
)

// cliRouteCase is one CLI-flag-shaped invocation of `dex query`, mirroring one
// rung of internal/mcp's routerShapeCorpus (router_accuracy_test.go) but
// exercised through the CLI's own flag→request path (buildQueryInput) rather
// than calling classifyQuery directly — the CLI-flag variant the #849 spec's
// Validation section asks for: proving the CLI's argument parsing reaches the
// same lane a raw MCP/REST QueryInput would, not a second classifier.
type cliRouteCase struct {
	name     string
	kind     string   // --kind, empty = infer from shape
	args     []string // positional args after flags, as a human would type them unquoted
	wantLane string
}

var cliRouterCorpus = []cliRouteCase{
	{name: "literal-path", args: []string{"internal/mcp/server.go"}, wantLane: "read"},
	{name: "path-range", args: []string{"internal/mcp/server.go:120-140"}, wantLane: "read"},
	{name: "path-line", args: []string{"internal/mcp/query.go:120"}, wantLane: "locate"},
	{name: "regex", args: []string{"/func .*Verb/"}, wantLane: "grep"},
	// route.lane reports "trace" (not the detected "symbol" shape) — it names
	// the populated result field (result.trace), matching dispatchExact's
	// remap for the symbol lane.
	{name: "bare-symbol", args: []string{"NewServer"}, wantLane: "trace"},
	// Unquoted multi-word prose must join into one input string, exactly like
	// the former `ask`/`search` verbs did — quoting stays optional on the CLI.
	{name: "behavior-prose-unquoted", args: []string{"how", "are", "edits", "debounced?"}, wantLane: "semantic"},
	{name: "forced-grep", args: []string{"anything", "at", "all"}, kind: "grep", wantLane: "grep"},
	{name: "forced-callers", args: []string{"NewServer"}, kind: "callers", wantLane: "trace"},
	{name: "forced-review", args: []string{"internal/x.go"}, kind: "review", wantLane: "review"},
	// The #849-added kinds: forced-only, no shape-detected route.
	{name: "forced-cohort", args: []string{"toolSurface"}, kind: "cohort", wantLane: "cohort"},
	{name: "forced-refs", args: []string{"NewServer"}, kind: "refs", wantLane: "refs"},
	{name: "forced-deps", args: []string{"internal/mcp"}, kind: "deps", wantLane: "deps"},
	{name: "forced-status", args: []string{}, kind: "status", wantLane: "status"},
}

// TestCLIQueryRouterAccuracy is the CLI-flag variant of the router-accuracy
// gate: for each case, build the QueryInput the way `dex query` actually
// would from parsed flags/positional args, then run it through the real
// (*Server).Query dispatcher and assert route.lane. No index is needed —
// route classification happens before any lane handler runs, so a bare temp
// project root is enough.
func TestCLIQueryRouterAccuracy(t *testing.T) {
	root := t.TempDir()
	srv, _ := newServerFromEnv(t.TempDir())
	ctx := context.Background()

	for _, c := range cliRouterCorpus {
		t.Run(c.name, func(t *testing.T) {
			in, err := buildQueryInput(c.kind, "", "", 0, 0, 0, false, root, c.args)
			if err != nil {
				t.Fatalf("buildQueryInput: %v", err)
			}
			out, err := srv.Query(ctx, in)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if out.Route.Lane != c.wantLane {
				t.Errorf("args=%v kind=%q → route.lane=%q, want %q (detected=%q)",
					c.args, c.kind, out.Route.Lane, c.wantLane, out.Route.Detected)
			}
		})
	}
}

// TestBuildQueryInputCheckClaims locks the one documented non-scalar-input
// exception (#849 spec, resolved open question #2): --kind=check treats every
// remaining positional as its own claim, not words of one joined string.
func TestBuildQueryInputCheckClaims(t *testing.T) {
	in, err := buildQueryInput("check", "", "", 0, 0, 0, false, "/proj",
		[]string{"internal/mcp/server.go:47", "internal/mcp/server.go:100:nonexistent"})
	if err != nil {
		t.Fatalf("buildQueryInput: %v", err)
	}
	if in.Input != "" {
		t.Errorf("check should not populate Input, got %q", in.Input)
	}
	if len(in.Claims) != 2 {
		t.Fatalf("want 2 claims, got %d: %+v", len(in.Claims), in.Claims)
	}
	if in.Claims[0].Ref != "internal/mcp/server.go:47" || in.Claims[1].Ref != "internal/mcp/server.go:100:nonexistent" {
		t.Errorf("claim refs = %+v, want the two positionals verbatim", in.Claims)
	}

	if _, err := buildQueryInput("check", "", "", 0, 0, 0, false, "/proj", nil); err == nil {
		t.Error("--kind=check with no positional refs should error, got nil")
	}
}
