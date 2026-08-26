package mcp

import (
	"context"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestQueryDispatchExactSymbolE2E is the integration test deferred from S2 (#196):
// until now only the semantic lane was exercised end-to-end (via http_mcp_test),
// while the exact and symbol lanes were covered only at the classifier level. The
// S5 cutover gate closes that gap — it drives query's read / slice / locate / grep /
// symbol lanes over a REAL indexed fixture and asserts each dispatches to its lane
// handler and returns real data with exact provenance, proving the shape→lane
// wiring holds through the whole stack, not just in classifyQuery's unit test.
func TestQueryDispatchExactSymbolE2E(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	// A caller→callee edge so the symbol (graph) lane has something to trace.
	src := "package main\n\n" +
		"func dispatchLeaf() int { return 7 }\n\n" +
		"func dispatchRoot() int { return dispatchLeaf() + 1 }\n"
	writeFile(t, filepath.Join(projDir, "main.go"), src)
	root := indexProject(t, projDir, cacheDir, srv.URL)
	h := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	call := func(t *testing.T, in QueryInput) QueryOutput {
		t.Helper()
		in.ProjectRoot = root
		_, out, err := queryVerb(ctx, h, &sdk.CallToolRequest{}, in)
		if err != nil {
			t.Fatalf("queryVerb(%q, kind=%q): %v", in.Input, in.Kind, err)
		}
		return out
	}

	t.Run("path→read/signatures", func(t *testing.T) {
		out := call(t, QueryInput{Input: "main.go"})
		if out.Route.Lane != "read" {
			t.Fatalf("lane = %q, want read (detected=%q)", out.Route.Lane, out.Route.Detected)
		}
		if out.Trust.Provenance != "exact" {
			t.Fatalf("provenance = %q, want exact", out.Trust.Provenance)
		}
		if out.Result.Read == nil {
			t.Fatalf("read lane returned no read result: %+v", out.Result)
		}
		// The uniform Selection currency (#95f) is populated for every lane.
		if len(out.Refs) == 0 || out.Refs[0].Kind != "file" || out.Refs[0].Prov != "exact" {
			t.Errorf("read lane should yield an exact file ref, got %+v", out.Refs)
		}
	})

	t.Run("range→read slice", func(t *testing.T) {
		out := call(t, QueryInput{Input: "main.go:3-3"})
		if out.Route.Lane != "read" {
			t.Fatalf("lane = %q, want read (detected=%q)", out.Route.Lane, out.Route.Detected)
		}
		if out.Result.Read == nil {
			t.Fatalf("range slice returned no read result: %+v", out.Result)
		}
	})

	t.Run("location→locate", func(t *testing.T) {
		out := call(t, QueryInput{Input: "main.go:3"})
		if out.Route.Lane != "locate" {
			t.Fatalf("lane = %q, want locate (detected=%q)", out.Route.Lane, out.Route.Detected)
		}
		if out.Result.Locate == nil {
			t.Fatalf("locate lane returned no locate result: %+v", out.Result)
		}
	})

	t.Run("regex→grep", func(t *testing.T) {
		out := call(t, QueryInput{Input: "/dispatchLeaf/"})
		if out.Route.Lane != "grep" {
			t.Fatalf("lane = %q, want grep (detected=%q)", out.Route.Lane, out.Route.Detected)
		}
		if out.Result.Grep == nil || len(out.Result.Grep.Matches) == 0 {
			t.Fatalf("grep lane returned no matches: %+v", out.Result)
		}
		if len(out.Refs) == 0 || out.Refs[0].Kind != "chunk" {
			t.Errorf("grep lane should yield chunk refs, got %+v", out.Refs)
		}
	})

	t.Run("symbol→graph/trace", func(t *testing.T) {
		out := call(t, QueryInput{Input: "dispatchLeaf"})
		// The symbol input shape (detected) routes to the trace lane on the wire
		// (#95g): route.lane names the populated field (result.trace).
		if out.Route.Detected != "symbol" {
			t.Fatalf("detected = %q, want symbol", out.Route.Detected)
		}
		if out.Route.Lane != "trace" {
			t.Fatalf("lane = %q, want trace (detected=%q)", out.Route.Lane, out.Route.Detected)
		}
		if out.Result.Trace == nil {
			t.Fatalf("symbol lane did not dispatch to trace: %+v", out.Result)
		}
		// Trace refs are symbols; provenance is exact or (partial recall) name-based.
		for _, rf := range out.Refs {
			if rf.Kind != "symbol" {
				t.Errorf("trace ref should be a symbol, got %+v", rf)
			}
		}
	})
}
