package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// allDispatchNames returns every name the registry claims main.go dispatches.
func allDispatchNames() []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, v.name)
	}
	return out
}

// metaCommands are dispatched in main.go's switch but are not real verbs
// (help/version aliases) — exempt from registry parity.
var metaCommands = map[string]bool{
	"-v": true, "-V": true, "--version": true, "-h": true, "--help": true, "help": true,
}

// dispatchCases extracts every command string from main_dispatch.go: string
// literals in `case "x":` clauses (version/help special cases) and string
// keys in the dispatch map (`"cmd": handler`). This is the actual command
// surface the binary responds to; the registry must mirror it exactly.
func dispatchCases(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main_dispatch.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main_dispatch.go: %v", err)
	}
	cases := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CaseClause:
			for _, expr := range x.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if s, err := strconv.Unquote(lit.Value); err == nil {
					cases[s] = true
				}
			}
		case *ast.KeyValueExpr:
			lit, ok := x.Key.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					cases[s] = true
				}
			}
		}
		return true
	})
	return cases
}

// TestRegistryMatchesDispatch is the anti-drift guard: it fails if main.go
// dispatches a command the registry omits, or the registry advertises a
// command main.go never dispatches. Either way usage text + completion would
// be wrong, so the build should break here (#469).
func TestRegistryMatchesDispatch(t *testing.T) {
	dispatched := dispatchCases(t)
	registered := map[string]bool{}
	for _, name := range allDispatchNames() {
		registered[name] = true
	}

	for cmd := range dispatched {
		if metaCommands[cmd] {
			continue
		}
		if !registered[cmd] {
			t.Errorf("main.go dispatches %q but it is not in the verb registry (registry.go)", cmd)
		}
	}
	for cmd := range registered {
		if !dispatched[cmd] {
			t.Errorf("registry advertises %q but main.go never dispatches it", cmd)
		}
	}
}

// TestVersionFlagAliasesDispatch covers #505: every version alias — the bare
// `version` verb and the -v/-V/--version flag forms — must be wired in the
// top-level dispatch so `dex -v` prints the version instead of "unknown command".
func TestVersionFlagAliasesDispatch(t *testing.T) {
	dispatched := dispatchCases(t)
	for _, alias := range []string{"version", "-v", "-V", "--version"} {
		if !dispatched[alias] {
			t.Errorf("top-level dispatch missing version alias %q", alias)
		}
	}
}

// TestMCPOnlyToolHint covers #521: the MCP-only hint seam must never return a
// hint for an unknown command, and no hinted name may drift into also being a
// registered CLI verb. The map is empty since #195 S4 removed its only member
// (session) from the MCP surface — the mechanism is retained for future tools.
func TestMCPOnlyToolHint(t *testing.T) {
	if _, ok := mcpOnlyToolHint("definitely-not-a-tool"); ok {
		t.Error("unknown command must not return an MCP-only hint")
	}
	// Any hinted name must reference a real MCP tool with no CLI verb — it must
	// NOT also be a registered CLI verb, or the hint is stale.
	registered := map[string]bool{}
	for _, name := range allDispatchNames() {
		registered[name] = true
	}
	for name, hint := range mcpOnlyToolHints {
		if registered[name] {
			t.Errorf("%q has an MCP-only hint but is a registered CLI verb", name)
		}
		if !strings.Contains(hint, "MCP") {
			t.Errorf("%q hint should mention MCP: %q", name, hint)
		}
	}
}

// TestCompletionCommandsNonEmpty guards the generator inputs.
func TestCompletionCommandsNonEmpty(t *testing.T) {
	if len(completionCommands()) == 0 {
		t.Fatal("completionCommands() is empty")
	}
	for _, v := range verbs {
		if v.name == "" {
			t.Error("verb with empty name in registry")
		}
		if v.group != groupHidden && v.summary == "" {
			t.Errorf("advertised verb %q has no summary", v.name)
		}
	}
}
