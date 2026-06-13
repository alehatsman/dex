package graph

// TypeScript tree-sitter extractor. Handles .ts and .tsx via the
// typescript grammar (mirrors internal/chunk/chunk.go's choice).
// Emits package / file / function / class / method / interface /
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
	"context"
	"path"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

func init() { Register(newTSExtractor) }

func newTSExtractor() Extractor {
	return &tsExtractor{jstsBase: jstsBase{
		lang:        "typescript",
		nodeIDs:     map[string]struct{}{},
		symbols:     map[string]map[string]string{},
		fileImports: map[string]*tsImportTable{},
		knownFiles:  map[string]string{},
	}}
}

type tsExtractor struct {
	jstsBase
}

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

// ---- Extractor interface ---------------------------------------------------

func (e *tsExtractor) Name() string               { return "typescript" }
func (e *tsExtractor) Language() *sitter.Language { return typescript.GetLanguage() }
func (e *tsExtractor) Extensions() []string       { return []string{".ts", ".tsx"} }

func (e *tsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg := tsPackagePath(in.RelPath)
	e.knownFiles[pkg] = in.RelPath

	pkgID := NodeID("", pkg, NodePackage, pkg)
	e.addNode(Node{
		ID:            pkgID,
		Kind:          NodePackage,
		Name:          path.Base(pkg),
		QualifiedName: pkg,
		PackagePath:   pkg,
		Metadata:      map[string]any{"language": "typescript"},
	})

	fileID := NodeID("", pkg, NodeFile, in.RelPath)
	e.addNode(Node{
		ID:            fileID,
		Kind:          NodeFile,
		Name:          filepath.Base(in.RelPath),
		QualifiedName: in.RelPath,
		PackagePath:   pkg,
		FilePath:      in.RelPath,
		StartLine:     1,
		EndLine:       lineOfPoint(in.Root.EndPoint().Row),
		Metadata:      map[string]any{"language": "typescript"},
	})
	e.edges = append(e.edges, Edge{
		ID:        EdgeID(pkgID, EdgeContains, fileID, in.RelPath, 1),
		Kind:      EdgeContains,
		SrcID:     pkgID,
		DstID:     fileID,
		FilePath:  in.RelPath,
		StartLine: 1,
		EndLine:   1,
	})

	imports := &tsImportTable{
		modules:     map[string]string{},
		fromImports: map[string]pyFromImport{},
	}
	e.fileImports[in.RelPath] = imports

	for i := 0; i < int(in.Root.NamedChildCount()); i++ {
		e.processTopLevel(in.Root.NamedChild(i), in.Source, in.RelPath, pkg, fileID, imports)
	}
	return nil
}

// ---- top-level processing --------------------------------------------------

// processTopLevel handles one direct child of the module root.
// export_statement is unwrapped to the underlying declaration so
// `export function foo` and `function foo` produce the same node
// shape; the export-ness is not currently tracked (consumers care
// about the symbol's existence, not its visibility).
func (e *tsExtractor) processTopLevel(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID string, imports *tsImportTable,
) {
	if n == nil {
		return
	}
	kind := n.Type()
	if kind == "export_statement" {
		// `export default ...` flows through the same path; the
		// `default` identifier in the symbol table is what makes
		// default-import resolution work.
		if decl := n.ChildByFieldName("declaration"); decl != nil {
			e.processTopLevel(decl, src, filePath, pkg, fileID, imports)
			e.maybeMarkDefaultExport(decl, src, pkg)
			return
		}
		// export { foo, bar } — re-exports; not modelled in first cut.
		return
	}
	switch kind {
	case "function_declaration":
		e.addFunction(n, src, filePath, pkg, fileID, "")
	case "class_declaration":
		e.addClass(n, src, filePath, pkg, fileID)
	case "interface_declaration":
		e.addInterface(n, src, filePath, pkg, fileID)
	case "lexical_declaration":
		e.addLexicalDecl(n, src, filePath, pkg, fileID)
	case "import_statement":
		e.parseImportStatement(n, src, filePath, pkg, fileID, imports)
	}
}

// ---- TS-specific declarations ---------------------------------------------

func (e *tsExtractor) addInterface(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID string,
) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, src)
	if name == "" {
		return
	}
	id := NodeID("", pkg, NodeInterface, name)
	startLine := lineOfPoint(n.StartPoint().Row)
	endLine := lineOfPoint(n.EndPoint().Row)
	if e.addNode(Node{
		ID:            id,
		Kind:          NodeInterface,
		Name:          name,
		QualifiedName: name,
		PackagePath:   pkg,
		FilePath:      filePath,
		StartLine:     startLine,
		EndLine:       endLine,
		Metadata:      map[string]any{"language": "typescript"},
	}) {
		e.symbols[pkg] = ensureMap(e.symbols[pkg])
		e.symbols[pkg][name] = id
	}
	e.edges = append(e.edges, Edge{
		ID:        EdgeID(fileID, EdgeContains, id, filePath, startLine),
		Kind:      EdgeContains,
		SrcID:     fileID,
		DstID:     id,
		FilePath:  filePath,
		StartLine: startLine,
		EndLine:   endLine,
	})
}

// ---- import parsing --------------------------------------------------------

// parseImportStatement handles every import shape that has a `from`:
//
//	import foo from "./p"
//	import { a, b as c } from "./p"
//	import * as ns from "./p"
//	import foo, { a } from "./p"
//	import "./p"                    (side-effect; no bindings)
//
// The source specifier is captured raw; resolution to a packagePath
// happens in Finalize once knownFiles is complete (the dispatch order
// across files isn't deterministic relative to source position).
func (e *tsExtractor) parseImportStatement(
	n *sitter.Node, src []byte,
	filePath, currentPkg, fileID string,
	imports *tsImportTable,
) {
	sourceNode := n.ChildByFieldName("source")
	if sourceNode == nil {
		return
	}
	specifier := stripQuotes(nodeText(sourceNode, src))
	if specifier == "" {
		return
	}
	startLine := lineOfPoint(n.StartPoint().Row)

	// Stash the "from" anchor for Finalize-time resolution. The
	// import_clause children give us local bindings; later we
	// translate each binding's pkg from raw specifier to resolved
	// packagePath. We borrow the modules map's "__from__" slot for
	// the per-file anchor — wiped after resolution.
	imports.modules["__from__"] = filePath

	clauseHandled := false
	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if child == nil || child == sourceNode {
			continue
		}
		if child.Type() != "import_clause" {
			continue
		}
		clauseHandled = true
		e.processImportClause(child, src, specifier, imports)
	}

	// Side-effect import (`import "./p"`) still emits the imports
	// edge so graph_deps can see the dependency. clauseHandled is
	// false in this case, but we always emit.
	_ = clauseHandled

	impID := NodeID("", currentPkg, NodeImport, specifier)
	e.addNode(Node{
		ID:            impID,
		Kind:          NodeImport,
		Name:          specifier,
		QualifiedName: specifier,
		PackagePath:   currentPkg,
		Metadata:      map[string]any{"language": "typescript"},
	})
	e.edges = append(e.edges, Edge{
		ID:        EdgeID(fileID, EdgeImports, impID, filePath, startLine),
		Kind:      EdgeImports,
		SrcID:     fileID,
		DstID:     impID,
		FilePath:  filePath,
		StartLine: startLine,
		EndLine:   startLine,
	})
	pkgID := NodeID("", currentPkg, NodePackage, currentPkg)
	e.edges = append(e.edges, Edge{
		ID:    EdgeID(pkgID, EdgeImports, impID, "", 0),
		Kind:  EdgeImports,
		SrcID: pkgID,
		DstID: impID,
	})
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
