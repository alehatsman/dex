package graph

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// Query-driven (tags) extractors for JavaScript and TypeScript. One
// tree-sitter query enumerates every definition / call / import; scope is
// recovered by walking each match's ancestors. The resolution layer
// (tsImportTable / resolveCall / Finalize, all on jstsBase) does the rest.
//
// The two languages share jstsBase and almost all logic; they differ only
// in grammar, extensions, package-path rule, whether interfaces exist, and
// whether `var` declarations bind arrow consts (js: yes, ts: no).

const jsTagsQuery = `
(function_declaration) @function
(class_declaration) @class
(method_definition) @method
(lexical_declaration) @lexdecl
(variable_declaration) @lexdecl
(import_statement) @import
(call_expression) @call
(new_expression) @call
(jsx_self_closing_element) @call
(jsx_opening_element) @call
`

// tsTagsQuery omits JSX patterns — the TypeScript grammar (as opposed to TSX)
// does not define jsx_* node types and will error at query compile time.
const tsTagsQuery = `
(function_declaration) @function
(class_declaration) @class
(interface_declaration) @interface
(method_definition) @method
(lexical_declaration) @lexdecl
(import_statement) @import
(call_expression) @call
(new_expression) @call
`

// jstsTagsExtractor is the JavaScript / TypeScript graph extractor. It
// embeds jstsBase — the resolver/emit base holding Init / Finalize /
// resolveCall / the emit helpers / import parsing — and drives it from a
// single tree-sitter query.
type jstsTagsExtractor struct {
	jstsBase
	langName string
	grammar  *sitter.Language
	exts     []string
	pkgPath  func(string) string
	queryStr string
	// declStmts is the set of declaration-statement node types whose
	// arrow/function bindings become function nodes (js also allows `var`).
	declStmts map[string]bool
	query     *sitter.Query
}

func newJSTagsExtractor() Extractor {
	return &jstsTagsExtractor{
		jstsBase:  newJSTSBase("javascript"),
		langName:  "javascript",
		grammar:   javascript.GetLanguage(),
		exts:      []string{".js", ".jsx"},
		pkgPath:   jsPackagePath,
		queryStr:  jsTagsQuery,
		declStmts: map[string]bool{"lexical_declaration": true, "variable_declaration": true},
	}
}

func newTSTagsExtractor() Extractor {
	return &jstsTagsExtractor{
		jstsBase:  newJSTSBase("typescript"),
		langName:  "typescript",
		grammar:   typescript.GetLanguage(),
		exts:      []string{".ts", ".tsx"},
		pkgPath:   tsPackagePath,
		queryStr:  tsTagsQuery,
		declStmts: map[string]bool{"lexical_declaration": true},
	}
}

func (e *jstsTagsExtractor) Name() string               { return e.langName }
func (e *jstsTagsExtractor) Language() *sitter.Language { return e.grammar }
func (e *jstsTagsExtractor) Extensions() []string       { return e.exts }

func (e *jstsTagsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg := e.pkgPath(in.RelPath)
	e.knownFiles[pkg] = in.RelPath
	fileID := e.emitScaffold(in, pkg)
	imports := &tsImportTable{
		modules:     map[string]string{},
		fromImports: map[string]pyFromImport{},
	}
	e.fileImports[in.RelPath] = imports

	if e.query == nil {
		q, err := sitter.NewQuery([]byte(e.queryStr), e.grammar)
		if err != nil {
			return fmt.Errorf("compile %s tags query: %w", e.langName, err)
		}
		e.query = q
	}

	var funcs, classes, ifaces, methods, lexdecls, imps, calls []*sitter.Node
	runTagsQuery(e.query, in.Root, func(capture string, n *sitter.Node) {
		switch capture {
		case "function":
			funcs = append(funcs, n)
		case "class":
			classes = append(classes, n)
		case "interface":
			ifaces = append(ifaces, n)
		case "method":
			methods = append(methods, n)
		case "lexdecl":
			lexdecls = append(lexdecls, n)
		case "import":
			imps = append(imps, n)
		case "call":
			calls = append(calls, n)
		}
	})

	// Imports — top-level only (direct program children), matching the
	// top-level processing.
	for _, n := range imps {
		if !jstsDeclTopLevel(n) {
			continue
		}
		e.parseImportStatement(n, in.Source, in.RelPath, pkg, fileID, imports)
	}

	// Classes — reachable from the module through class/export nesting
	// only (module → class → class nesting, never into
	// function bodies).
	for _, n := range classes {
		if !jstsClassReachable(n) {
			continue
		}
		className := e.emitClassNode(n, in.Source, in.RelPath, pkg, fileID)
		e.collectClassFieldTypes(n, in.Source, pkg, className)
		e.maybeMarkDefaultExport(n, in.Source, pkg)
	}

	// Top-level functions and interfaces.
	for _, n := range funcs {
		if !jstsDeclTopLevel(n) {
			continue
		}
		e.emitFunctionNode(n, in.Source, in.RelPath, pkg, fileID, "")
		e.maybeMarkDefaultExport(n, in.Source, pkg)
	}
	for _, n := range ifaces {
		if !jstsDeclTopLevel(n) {
			continue
		}
		e.addInterface(n, in.Source, in.RelPath, pkg, fileID)
		e.maybeMarkDefaultExport(n, in.Source, pkg)
	}

	// Methods — direct members of a reachable class.
	for _, n := range methods {
		className, ok := jstsMethodClass(n, in.Source)
		if !ok {
			continue
		}
		e.emitFunctionNode(n, in.Source, in.RelPath, pkg, fileID, className)
	}

	// Arrow/function consts — top-level declaration statements only.
	for _, n := range lexdecls {
		if !e.declStmts[n.Type()] || !jstsDeclTopLevel(n) {
			continue
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			e.emitArrowDeclarator(n, n.NamedChild(i), in.Source, in.RelPath, pkg, fileID)
		}
		e.maybeMarkDefaultExport(n, in.Source, pkg)
	}

	// Calls — attributed to the enclosing node-function's body (stop at
	// nested def/class; node-functions only; body subtree only).
	for _, n := range calls {
		e.collectQueryCall(n, in.Source, in.RelPath, pkg)
	}
	return nil
}

// collectQueryCall attributes a single call to its enclosing function
// (call_expression / new_expression) discovered by the query.
func (e *jstsTagsExtractor) collectQueryCall(n *sitter.Node, src []byte, filePath, pkg string) {
	// Walk up to the nearest enclosing function-like node that is a graph
	// node. Function-like ancestors that are NOT graph nodes (object-literal
	// getters/methods, callbacks, IIFEs) are transparent: a call lexically
	// nested inside them is attributed to the enclosing named function — e.g.
	// `extend` calls `assignProp` inside a `get shape()` getter on an object
	// literal, and that edge belongs to `extend`. A class_declaration boundary
	// stops the walk so class-body field initializers (never collected) are
	// dropped, not attributed to an enclosing function.
	var boundary *sitter.Node
	var callerID, callerCls string
	for anc := firstAncestorOfType(n,
		"function_declaration", "method_definition",
		"arrow_function", "function", "function_expression",
		"class_declaration"); anc != nil; anc = firstAncestorOfType(anc,
		"function_declaration", "method_definition",
		"arrow_function", "function", "function_expression",
		"class_declaration") {
		if anc.Type() == "class_declaration" {
			return
		}
		if id, cls, ok := e.jstsCallerInfo(anc, src, pkg); ok {
			boundary, callerID, callerCls = anc, id, cls
			break
		}
	}
	if boundary == nil {
		return
	}
	// The call must sit in the function's body, not its parameter list.
	if body := boundary.ChildByFieldName("body"); !nodeContains(body, n) {
		return
	}

	var callee *sitter.Node
	switch n.Type() {
	case "call_expression":
		callee = n.ChildByFieldName("function")
	case "new_expression":
		callee = n.ChildByFieldName("constructor")
	case "jsx_self_closing_element", "jsx_opening_element":
		callee = n.ChildByFieldName("name")
	}
	if callee == nil {
		return
	}
	expr := classifyTSCallee(callee, src)
	if expr.kind == "skip" {
		return
	}
	e.pendingCalls = append(e.pendingCalls, tsPendingCall{
		callerID:   callerID,
		callerPkg:  pkg,
		callerCls:  callerCls,
		calleeExpr: expr,
		filePath:   filePath,
		line:       lineOfPoint(n.StartPoint().Row),
	})
}

// jstsCallerInfo returns the node ID (and class, for methods) of a
// function-like node IF it is a graph node whose body's calls are
// collected. Returns ok=false otherwise.
func (e *jstsTagsExtractor) jstsCallerInfo(fn *sitter.Node, src []byte, pkg string) (id, cls string, ok bool) {
	switch fn.Type() {
	case "function_declaration":
		if !jstsDeclTopLevel(fn) {
			return "", "", false
		}
		name := nodeText(fn.ChildByFieldName("name"), src)
		if name == "" {
			return "", "", false
		}
		return NodeID("", pkg, NodeFunction, name), "", true

	case "method_definition":
		className, mok := jstsMethodClass(fn, src)
		if !mok {
			return "", "", false
		}
		name := nodeText(fn.ChildByFieldName("name"), src)
		if name == "" {
			return "", "", false
		}
		return NodeID("", pkg, NodeMethod, className+"."+name), className, true

	case "arrow_function", "function", "function_expression":
		declr := fn.Parent()
		if declr == nil || declr.Type() != "variable_declarator" {
			return "", "", false
		}
		decl := declr.Parent()
		if decl == nil || !e.declStmts[decl.Type()] || !jstsDeclTopLevel(decl) {
			return "", "", false
		}
		name := nodeText(declr.ChildByFieldName("name"), src)
		if name == "" {
			return "", "", false
		}
		return NodeID("", pkg, NodeFunction, name), "", true
	}
	return "", "", false
}

// jstsDeclTopLevel reports whether n is a top-level statement: a direct
// child of the tree root, or the declaration of a top-level
// export_statement.
//
// "Top-level" is defined as "direct child of the root node", matching the
// "top-level" = direct child of the tree root. The root is usually a
// program node, but on a malformed parse (e.g. a .tsx file run through the
// non-JSX TypeScript grammar) the root is an ERROR node whose children are
// the still-valid leading declarations; those are captured too.
func jstsDeclTopLevel(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	if isTreeRoot(p) {
		return true
	}
	return p.Type() == "export_statement" && isTreeRoot(p.Parent())
}

// isTreeRoot reports whether n is the tree's root node (the only node
// without a parent).
func isTreeRoot(n *sitter.Node) bool {
	return n != nil && n.Parent() == nil
}

// jstsClassReachable reports whether this class is reachable from the
// module through class/export nesting only: every ancestor up to the
// program must be a class/export/program node (never a function, block, or
// statement) — i.e. module → class → nested class.
func jstsClassReachable(classDecl *sitter.Node) bool {
	for p := classDecl.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "program":
			return true
		case "export_statement", "class_declaration", "class_body", "ERROR":
			continue
		default:
			return false
		}
	}
	// Walked off the top through allowed nodes only (e.g. an ERROR root on
	// a malformed parse) — the class is reachable.
	return true
}

// jstsMethodClass returns the enclosing class name for a method_definition
// that is a direct member of a reachable class, and ok=false otherwise.
func jstsMethodClass(method *sitter.Node, src []byte) (string, bool) {
	parent := method.Parent()
	if parent == nil || parent.Type() != "class_body" {
		return "", false
	}
	classDecl := parent.Parent()
	if classDecl == nil || classDecl.Type() != "class_declaration" || !jstsClassReachable(classDecl) {
		return "", false
	}
	className := nodeText(classDecl.ChildByFieldName("name"), src)
	if className == "" {
		return "", false
	}
	return className, true
}
