package graph

// jstsBase holds state and methods shared between jsExtractor
// (sitter_javascript.go) and tsExtractor (sitter_ts.go). Concrete
// types embed this struct and differ only in grammar, file
// extensions, package-path derivation, and a handful of TS-specific
// node types (interface_declaration, addInterface, addNode promotion).

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

type jstsBase struct {
	lang string // "javascript" or "typescript"

	projectRoot  string
	nodes        []Node
	edges        []Edge
	nodeIDs      map[string]struct{}
	symbols      map[string]map[string]string
	fileImports  map[string]*tsImportTable
	knownFiles   map[string]string
	pendingCalls []tsPendingCall
	warnings     []string
}

func (e *jstsBase) Init(_ context.Context, root string) error {
	e.projectRoot = root
	return nil
}

func (e *jstsBase) Finalize(_ context.Context) ([]Node, []Edge, []string, error) {
	// Module specifiers were captured as raw strings during the walk;
	// resolve them now that knownFiles is complete (a forward reference
	// would otherwise miss because of walk order).
	for _, imp := range e.fileImports {
		for local, target := range imp.modules {
			if local == "__from__" {
				continue
			}
			imp.modules[local] = e.resolveModuleSpecifier(target, imp.modules["__from__"])
		}
		for local, fi := range imp.fromImports {
			fi.pkg = e.resolveModuleSpecifier(fi.pkg, imp.modules["__from__"])
			imp.fromImports[local] = fi
		}
		delete(imp.modules, "__from__")
	}

	for _, c := range e.pendingCalls {
		dst := e.resolveCall(c)
		if dst == "" {
			continue
		}
		e.edges = append(e.edges, Edge{
			ID:        EdgeID(c.callerID, EdgeCalls, dst, c.filePath, c.line),
			Kind:      EdgeCalls,
			SrcID:     c.callerID,
			DstID:     dst,
			FilePath:  c.filePath,
			StartLine: c.line,
			EndLine:   c.line,
		})
	}
	return e.nodes, e.edges, e.warnings, nil
}

// addFunction registers a top-level function OR a class method.
// Container symmetry with the Python extractor: when className is
// non-empty the node gets `has_method` from the class.
func (e *jstsBase) addFunction(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID, className string,
) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, src)
	if name == "" {
		return
	}
	kind := NodeFunction
	qn := name
	if className != "" {
		kind = NodeMethod
		qn = className + "." + name
	}
	id := NodeID("", pkg, kind, qn)
	startLine := lineOfPoint(n.StartPoint().Row)
	endLine := lineOfPoint(n.EndPoint().Row)
	meta := map[string]any{"language": e.lang}
	if className != "" {
		meta["receiver"] = className
	}
	if e.addNode(Node{
		ID:            id,
		Kind:          kind,
		Name:          name,
		QualifiedName: qn,
		PackagePath:   pkg,
		FilePath:      filePath,
		StartLine:     startLine,
		EndLine:       endLine,
		Metadata:      meta,
	}) {
		e.symbols[pkg] = ensureMap(e.symbols[pkg])
		e.symbols[pkg][qn] = id
		if className == "" {
			e.symbols[pkg][name] = id
		}
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
	if className != "" {
		clsID := NodeID("", pkg, NodeClass, className)
		e.edges = append(e.edges, Edge{
			ID:        EdgeID(clsID, EdgeHasMethod, id, filePath, startLine),
			Kind:      EdgeHasMethod,
			SrcID:     clsID,
			DstID:     id,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   endLine,
		})
	}
	if body := n.ChildByFieldName("body"); body != nil {
		e.collectCalls(body, src, id, pkg, className, filePath)
	}
}

// addClass registers a class node and recurses into its body for
// methods. Nested classes are treated as top-level; their QN doesn't
// include the outer class (matches Python's first-cut).
func (e *jstsBase) addClass(
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
	id := NodeID("", pkg, NodeClass, name)
	startLine := lineOfPoint(n.StartPoint().Row)
	endLine := lineOfPoint(n.EndPoint().Row)
	if e.addNode(Node{
		ID:            id,
		Kind:          NodeClass,
		Name:          name,
		QualifiedName: name,
		PackagePath:   pkg,
		FilePath:      filePath,
		StartLine:     startLine,
		EndLine:       endLine,
		Metadata:      map[string]any{"language": e.lang},
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

	body := n.ChildByFieldName("body")
	if body == nil {
		return
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "method_definition":
			e.addFunction(child, src, filePath, pkg, fileID, name)
		case "class_declaration":
			e.addClass(child, src, filePath, pkg, fileID)
		}
	}
}

// addLexicalDecl handles `const foo = () => {}` and `const foo =
// function(){}`. Other const forms (object/string literals) are
// ignored — they're not callable in a graph-meaningful way. Multiple
// declarators in one statement (`const a=…, b=…`) each emit their
// own node.
func (e *jstsBase) addLexicalDecl(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID string,
) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if child == nil || child.Type() != "variable_declarator" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		valueNode := child.ChildByFieldName("value")
		if nameNode == nil || valueNode == nil {
			continue
		}
		name := nodeText(nameNode, src)
		if name == "" {
			continue
		}
		// Only function-like values become function nodes.
		switch valueNode.Type() {
		case "arrow_function", "function", "function_expression":
		default:
			continue
		}
		id := NodeID("", pkg, NodeFunction, name)
		startLine := lineOfPoint(n.StartPoint().Row)
		endLine := lineOfPoint(n.EndPoint().Row)
		if e.addNode(Node{
			ID:            id,
			Kind:          NodeFunction,
			Name:          name,
			QualifiedName: name,
			PackagePath:   pkg,
			FilePath:      filePath,
			StartLine:     startLine,
			EndLine:       endLine,
			Metadata:      map[string]any{"language": e.lang, "form": "arrow"},
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
		if body := valueNode.ChildByFieldName("body"); body != nil {
			e.collectCalls(body, src, id, pkg, "", filePath)
		}
	}
}

// maybeMarkDefaultExport sets symbols[pkg]["default"] to the node ID
// of the just-emitted declaration when the wrapping export_statement
// declared it as the default.
func (e *jstsBase) maybeMarkDefaultExport(decl *sitter.Node, src []byte, pkg string) {
	parent := decl.Parent()
	if parent == nil || parent.Type() != "export_statement" {
		return
	}
	hasDefault := false
	for i := 0; i < int(parent.ChildCount()); i++ {
		c := parent.Child(i)
		if c != nil && c.Type() == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		return
	}
	if nameNode := decl.ChildByFieldName("name"); nameNode != nil {
		name := nodeText(nameNode, src)
		if id := e.symbolIn(pkg, name); id != "" {
			e.symbols[pkg] = ensureMap(e.symbols[pkg])
			e.symbols[pkg]["default"] = id
		}
	}
}

// processImportClause walks an import_clause and seeds the import
// table with each binding. specifier is the raw "./foo" form — it's
// stored as-is and rewritten to a packagePath in Finalize.
func (e *jstsBase) processImportClause(
	clause *sitter.Node, src []byte,
	specifier string, imports *tsImportTable,
) {
	for i := 0; i < int(clause.NamedChildCount()); i++ {
		child := clause.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "identifier":
			// Default import: `import Foo from "./p"`.
			local := nodeText(child, src)
			if local != "" {
				imports.fromImports[local] = pyFromImport{pkg: specifier, name: "default"}
			}
		case "namespace_import":
			// `import * as X from "./p"` — X is bound to the module.
			aliasNode := child.ChildByFieldName("alias")
			if aliasNode == nil {
				aliasNode = lastNamedChild(child)
			}
			if aliasNode != nil {
				if local := nodeText(aliasNode, src); local != "" {
					imports.modules[local] = specifier
				}
			}
		case "named_imports":
			for j := 0; j < int(child.NamedChildCount()); j++ {
				spec := child.NamedChild(j)
				if spec == nil || spec.Type() != "import_specifier" {
					continue
				}
				nameNode := spec.ChildByFieldName("name")
				aliasNode := spec.ChildByFieldName("alias")
				if nameNode == nil {
					continue
				}
				original := nodeText(nameNode, src)
				local := original
				if aliasNode != nil {
					local = nodeText(aliasNode, src)
				}
				if original == "" || local == "" {
					continue
				}
				imports.fromImports[local] = pyFromImport{pkg: specifier, name: original}
			}
		}
	}
}

// collectCalls walks `body` for call_expression and new_expression,
// stopping descent at nested function/class definitions.
func (e *jstsBase) collectCalls(
	body *sitter.Node, src []byte,
	callerID, callerPkg, callerCls, filePath string,
) {
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "function_declaration", "class_declaration",
			"arrow_function", "function", "function_expression",
			"method_definition":
			return
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn != nil {
				expr := classifyTSCallee(fn, src)
				if expr.kind != "skip" {
					e.pendingCalls = append(e.pendingCalls, tsPendingCall{
						callerID:   callerID,
						callerPkg:  callerPkg,
						callerCls:  callerCls,
						calleeExpr: expr,
						filePath:   filePath,
						line:       lineOfPoint(n.StartPoint().Row),
					})
				}
			}
		case "new_expression":
			ctor := n.ChildByFieldName("constructor")
			if ctor != nil {
				expr := classifyTSCallee(ctor, src)
				if expr.kind != "skip" {
					e.pendingCalls = append(e.pendingCalls, tsPendingCall{
						callerID:   callerID,
						callerPkg:  callerPkg,
						callerCls:  callerCls,
						calleeExpr: expr,
						filePath:   filePath,
						line:       lineOfPoint(n.StartPoint().Row),
					})
				}
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
}

func (e *jstsBase) resolveCall(c tsPendingCall) string {
	switch c.calleeExpr.kind {
	case "bare":
		name := c.calleeExpr.parts[0]
		if id := e.symbolIn(c.callerPkg, name); id != "" {
			return id
		}
		imports := e.fileImports[c.filePath]
		if imports == nil {
			return ""
		}
		if fi, ok := imports.fromImports[name]; ok {
			return e.symbolIn(fi.pkg, fi.name)
		}
		return ""

	case "self":
		if c.callerCls == "" || len(c.calleeExpr.parts) < 2 {
			return ""
		}
		methodName := c.calleeExpr.parts[1]
		return e.symbolIn(c.callerPkg, c.callerCls+"."+methodName)

	case "attr":
		imports := e.fileImports[c.filePath]
		if imports == nil {
			return ""
		}
		head := c.calleeExpr.parts[0]
		tail := c.calleeExpr.parts[1:]
		if len(tail) == 0 {
			return ""
		}
		if mod, ok := imports.modules[head]; ok {
			if len(tail) != 1 {
				return ""
			}
			return e.symbolIn(mod, tail[0])
		}
		if fi, ok := imports.fromImports[head]; ok {
			if len(tail) == 1 {
				return e.symbolIn(fi.pkg, fi.name+"."+tail[0])
			}
			return ""
		}
		return ""
	}
	return ""
}

func (e *jstsBase) symbolIn(pkg, name string) string {
	bucket := e.symbols[pkg]
	if bucket == nil {
		return ""
	}
	return bucket[name]
}

// resolveModuleSpecifier turns a raw specifier (./foo, ../bar/baz,
// react) into the packagePath of the file it refers to, or returns
// the original string when the specifier doesn't resolve to a known
// project file. The dir+`/index` fallback covers barrel modules.
func (e *jstsBase) resolveModuleSpecifier(specifier, fromFile string) string {
	if specifier == "" {
		return ""
	}
	if !strings.HasPrefix(specifier, ".") {
		return specifier
	}
	if fromFile == "" {
		return specifier
	}
	base := path.Dir(filepath.ToSlash(fromFile))
	joined := path.Clean(path.Join(base, specifier))
	if _, ok := e.knownFiles[joined]; ok {
		return joined
	}
	if _, ok := e.knownFiles[joined+"/index"]; ok {
		return joined + "/index"
	}
	return specifier
}

func (e *jstsBase) addNode(n Node) bool {
	if _, ok := e.nodeIDs[n.ID]; ok {
		return false
	}
	e.nodeIDs[n.ID] = struct{}{}
	e.nodes = append(e.nodes, n)
	return true
}
