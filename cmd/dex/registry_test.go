package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// allDispatchNames returns every name the registry claims main.go dispatches:
// canonical names plus aliases.
func allDispatchNames() []string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, v.name)
		out = append(out, v.aliases...)
	}
	return out
}

// metaCommands are dispatched in main.go's switch but are not real verbs
// (help/version aliases) — exempt from registry parity.
var metaCommands = map[string]bool{
	"-V": true, "--version": true, "-h": true, "--help": true, "help": true,
}

// dispatchCases extracts every string literal in a `case "x":` clause of the
// top-level switch inside func main(). This is the actual command surface the
// binary responds to; the registry must mirror it exactly.
func dispatchCases(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	cases := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range cc.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil {
				cases[s] = true
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
