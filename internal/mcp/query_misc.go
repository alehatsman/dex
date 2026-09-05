package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// dispatchMisc routes the four forced-kind-only lanes folded into query by the
// CLI collapse (#849): check, refs (xref), cohort, deps, status. Each already
// existed as its own single-input server verb; this is a thin adapter, not new
// retrieval logic. Unlike dispatchExact/dispatchSemantic these lanes have no
// shape-detected route — kindToLane only reaches them via an explicit `kind`.
func dispatchMisc(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, lr laneRoute, cleaned string, route QueryRoute) (*sdk.CallToolResult, QueryOutput, error) {
	switch lr.lane {
	case "check":
		_, co, err := h.check(ctx, req, CheckInput{ProjectRoot: in.ProjectRoot, Claims: in.Claims})
		out := QueryOutput{Status: checkEnvelopeStatus(co), Route: route, Result: QueryResult{Check: &co}}
		return nil, out, err

	case "refs":
		ro, err := dispatchRefs(ctx, h, req, in, cleaned)
		out := QueryOutput{Status: ro.Status, Hint: ro.Hint, Route: route, Result: QueryResult{Xref: &ro}}
		return nil, out, err

	case "cohort":
		_, co, err := h.cohort(ctx, req, CohortInput{Interface: cleaned, ProjectRoot: in.ProjectRoot})
		out := QueryOutput{Status: co.Status, Hint: co.Hint, Route: route, Result: QueryResult{Cohort: &co}}
		return nil, out, err

	case "deps":
		path, pkg := inferDepsTarget(in.ProjectRoot, cleaned)
		_, do, err := h.graphDeps(ctx, req, GraphDepsInput{Path: path, Package: pkg, ProjectRoot: in.ProjectRoot})
		out := QueryOutput{Status: do.Status, Hint: do.Hint, Route: route, Result: QueryResult{Deps: &do}}
		return nil, out, err

	case "status":
		_, so, err := h.status(ctx, req, StatusInput{})
		out := QueryOutput{Status: "ok", Route: route, Result: QueryResult{StatusReport: &so}}
		return nil, out, err
	}
	return nil, QueryOutput{Status: "error", Route: route, Hint: "unreachable lane " + lr.lane}, nil
}

// checkEnvelopeStatus projects CheckOutput's per-claim results onto the
// envelope's single status string: "ok" only when every claim verified clean,
// "error" if any claim moved/vanished/failed to parse — mirroring cmdCheck's
// former exit-code rule (checkStatusFailed) now that check has no CLI-local
// exit code of its own to carry the same signal.
func checkEnvelopeStatus(co CheckOutput) string {
	for _, r := range co.Results {
		switch r.Status {
		case "moved", "gone", "no_file", "parse_error":
			return "error"
		}
	}
	return "ok"
}

// dispatchRefs calls the refs verb, defaulting Want to the "references" action
// when unset (matching refs' own default) so `query(input=<symbol>, kind=refs)`
// with no want is a valid, useful call, not a required-field error.
func dispatchRefs(ctx context.Context, h toolSurface, req *sdk.CallToolRequest, in QueryInput, symbol string) (RefsOutput, error) {
	_, ro, err := h.refs(ctx, req, RefsInput{Action: in.Want, Symbol: symbol, ProjectRoot: in.ProjectRoot})
	return ro, err
}

// inferDepsTarget maps a `query(kind=deps)` input to either a relative file
// Path or a full import Package, mirroring how GraphDeps resolves each (Path →
// NodesByPath, Package → NodesByPackage). Ported verbatim from the CLI's former
// `graph deps` subcommand (cmd/dex/graph_deps.go) so the folded-in kind behaves
// identically: an existing file is a Path; an existing directory is a package
// dir resolved to a representative .go file; anything not on disk is a full
// import path (Package).
func inferDepsTarget(projRoot, target string) (path, pkg string) {
	abs := target
	if !filepath.IsAbs(abs) && projRoot != "" {
		abs = filepath.Join(projRoot, target)
	}
	info, err := os.Stat(abs)
	switch {
	case err == nil && info.IsDir():
		if f := firstGoFile(abs); f != "" {
			if rel, rerr := filepath.Rel(projRoot, f); rerr == nil {
				return rel, ""
			}
		}
		return target, ""
	case err == nil:
		if rel, rerr := filepath.Rel(projRoot, abs); rerr == nil {
			return rel, ""
		}
		return target, ""
	default:
		return "", target
	}
}

// firstGoFile returns a representative .go file in dir (non-test preferred), or
// "" if none. os.ReadDir is name-sorted, so the choice is deterministic.
func firstGoFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var testFallback string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			if testFallback == "" {
				testFallback = filepath.Join(dir, e.Name())
			}
			continue
		}
		return filepath.Join(dir, e.Name())
	}
	return testFallback
}
