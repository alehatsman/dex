package graph

// TypeScript tree-sitter extractor. Handles .ts via the plain TypeScript
// grammar; .tsx gets its own extractor (newTSXTagsExtractor,
// sitter_jsts_tags.go) on the JSX-aware TSX grammar — the plain grammar
// mislexes JSX syntax as type assertions (#232).
// Emits package / file / function / class / method / interface / type /
// import nodes and contains / has_method / imports / calls edges
// with the same shape as the Python extractor — graph_deps and
// graph_callers see a unified surface across languages.
//
// Package-path encoding: forward-slash file path minus extension.
// `src/foo.ts` → "src/foo"; `src/foo/index.ts` → "src/foo" (matches
// the specifier `import './foo'` reaches). This differs from
// Python's dot-form on purpose: TS uses path-based module specifiers,
// not dotted module names, and matching the specifier shape makes
// import resolution a slice-and-join instead of a string rewrite.
//
// Resolution scope (single Run, single package per file):
//   - Bare calls / `new Foo()`     — same-file or `import { X } from './p'`
//   - `ns.member()`                — `import * as ns from './p'`
//   - `this.method()` in a method  — same class
//   - `import { default as d }` / `import d from './p'` — default
//     export tracked via a synthetic "default" slot in the symbol
//     table when the source file has one export declaration.
//
// What gets skipped: dynamic imports, JSX components used as
// callables (their resolution is component-by-import-name and
// works naturally for upper-case identifiers; lower-case JSX is
// HTML), type-only imports, decorators, and generics. Same trade
// as Python — best-effort by name; precision is the LSP lane.
//
// Shared state and methods live in jstsBase (sitter_jsts.go).

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// tsImportTable per-file: each local binding maps to either an
// imported file (for `import * as X from './p'`) or a (file,
// originalName) pair (for named or default imports). modules and
// fromImports mirror Python's layout so the call-resolution code is
// structurally parallel.
type tsImportTable struct {
	// modules: localName → packagePath of the imported file. Used for
	// namespace imports: `import * as X from './p'` ⇒ {"X": "p"}.
	// Default imports ALSO land here when the import shape's source
	// has been resolved — calling the default like `Foo()` resolves
	// via the symbol table's "default" slot.
	modules map[string]string

	// fromImports: localName → (packagePath, originalName). Used for
	// named imports: `import { foo as f } from './p'` ⇒
	// {"f": ("p", "foo")}.
	fromImports map[string]pyFromImport
}

type tsPendingCall struct {
	callerID   string
	callerPkg  string
	callerCls  string // "" outside methods
	calleeExpr pyCallee
	filePath   string
	line       int
}

// ---- helpers ---------------------------------------------------------------

// classifyTSCallee mirrors classifyCallee for Python. `this` is the
// TS analogue of `self`; flattening member_expression chains gives us
// `a.b.c` as parts.
func classifyTSCallee(n *sitter.Node, src []byte) pyCallee {
	switch n.Type() {
	case "identifier":
		return pyCallee{kind: "bare", parts: []string{nodeText(n, src)}}
	case "member_expression":
		parts := flattenTSMember(n, src)
		if len(parts) < 2 {
			return pyCallee{kind: "skip"}
		}
		if parts[0] == "this" {
			return pyCallee{kind: "self", parts: parts}
		}
		return pyCallee{kind: "attr", parts: parts}
	}
	return pyCallee{kind: "skip"}
}

// flattenTSMember turns `a.b.c` (parsed as member_expression
// (member_expression a b) c) into ["a","b","c"]. Returns nil for any
// chain that has a non-identifier base (calls, indexes, etc.) — we
// don't try to resolve those.
func flattenTSMember(n *sitter.Node, src []byte) []string {
	var out []string
	cur := n
	for cur != nil && cur.Type() == "member_expression" {
		prop := cur.ChildByFieldName("property")
		obj := cur.ChildByFieldName("object")
		if prop == nil || obj == nil {
			return nil
		}
		out = append([]string{nodeText(prop, src)}, out...)
		cur = obj
	}
	if cur == nil {
		return nil
	}
	switch cur.Type() {
	case "identifier":
		out = append([]string{nodeText(cur, src)}, out...)
	case "this":
		out = append([]string{"this"}, out...)
	default:
		return nil
	}
	return out
}

// tsPackagePath strips the extension from a .ts/.tsx file path. The
// `index.ts(x)` filename is kept verbatim in the package path —
// resolveModuleSpecifier handles the dir-vs-index disambiguation by
// probing both forms.
func tsPackagePath(relPath string) string {
	p := filepath.ToSlash(relPath)
	for _, ext := range []string{".tsx", ".ts"} {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext)
		}
	}
	return p
}

func stripQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') || (first == '`' && last == '`') {
		return s[1 : len(s)-1]
	}
	return s
}

func lastNamedChild(n *sitter.Node) *sitter.Node {
	c := int(n.NamedChildCount())
	if c == 0 {
		return nil
	}
	return n.NamedChild(c - 1)
}
