package mcp

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// restRouteCeiling is the maximum number of REST routes buildHTTPHandler may
// register (spec Validation, #851: "a test asserting the registered route
// count for /v1/projects/{id}/* matches the post-collapse list exactly").
// Set to the measured post-collapse baseline (ask/map/locate/cohort/refs/
// deps/clones folded into /query). Lowering it is the point as more routes
// earn a fold; raising it needs a reviewed reason — mirrors
// antiAccretionCeiling's discipline (anti_accretion_test.go).
const restRouteCeiling = 24

// restRoutes parses http.go's buildHTTPHandler and extracts every route
// pattern registered via mux.HandleFunc / authed.HandleFunc / authed.Handle,
// in source order. Parsing the source (rather than spinning up a real
// listener and reflecting on the mux) keeps the ratchet exact and immune to
// net/http.ServeMux internals, mirroring cmd/dex/registry_test.go's
// dispatchCases approach for the CLI's own dispatch table.
func restRoutes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "http.go", nil, 0)
	if err != nil {
		t.Fatalf("parse http.go: %v", err)
	}
	var routes []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || (recv.Name != "mux" && recv.Name != "authed") {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := parseStringLit(lit.Value)
		if err == nil && pattern != "/v1/" { // the outer auth-middleware mount, not a route
			routes = append(routes, pattern)
		}
		return true
	})
	return routes
}

func parseStringLit(v string) (string, error) {
	if len(v) >= 2 {
		return v[1 : len(v)-1], nil
	}
	return "", errors.New("not a quoted string literal")
}

// TestRESTRouteCeiling guards the #851 collapse: the registered route count
// must not creep back up. A regression here means a route was added without
// a reviewed reason to raise restRouteCeiling, or a folded route was
// resurrected instead of routed through /query's kind= ladder.
func TestRESTRouteCeiling(t *testing.T) {
	routes := restRoutes(t)
	t.Logf("REST routes (%d): %v", len(routes), routes)
	if len(routes) > restRouteCeiling {
		t.Errorf("registered REST route count = %d, exceeds ceiling %d — "+
			"a new route was added outright, or a folded route came back, "+
			"without raising restRouteCeiling with a reviewed reason",
			len(routes), restRouteCeiling)
	}
	// The single query route must exist — the whole point of the collapse is
	// that it's the front door for every kind= value.
	found := false
	for _, r := range routes {
		if r == "POST /v1/projects/{id}/query" {
			found = true
			break
		}
	}
	if !found {
		t.Error("POST /v1/projects/{id}/query is not registered")
	}
	// The folded routes must NOT be back.
	for _, folded := range []string{
		"POST /v1/projects/{id}/ask",
		"POST /v1/projects/{id}/map",
		"POST /v1/projects/{id}/locate",
		"POST /v1/projects/{id}/cohort",
		"POST /v1/projects/{id}/refs",
		"POST /v1/projects/{id}/deps",
		"POST /v1/projects/{id}/clones",
	} {
		for _, r := range routes {
			if r == folded {
				t.Errorf("folded route %q is registered again — should be reachable via /query kind= instead", folded)
			}
		}
	}
}
