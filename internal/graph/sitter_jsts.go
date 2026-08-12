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

	"github.com/alehatsman/dex/internal/graph/resolve"
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
	// fieldTypes records a class's instance-field types for DI/adapter
	// dispatch: classKey (pkg + "\x00" + className) → fieldName → the
	// simple type-name the field holds (a class name, same-file or
	// imported). Populated from constructor parameter-properties, typed
	// field declarations, and `field = new T()` initializers. Lets
	// `this.field.method()` resolve to `T.method` name-based.
	fieldTypes map[string]map[string]string
	// reExports records barrel re-exports (`export … from './x'`) per module
	// packagePath, so a cross-package call whose import lands on an index.ts
	// that only re-exports the definition still binds (#127 Phase 2). Raw
	// specifiers are captured during the walk and resolved in Finalize, mirroring
	// fileImports. Nil bucket is safe — resolveExport guards it.
	reExports map[string]*reExportTable
	warnings  []string
	// workspace resolves non-relative specifiers (@acme/*, @/*) to project
	// files via package.json names + tsconfig path aliases. Built once at the
	// top of Finalize (projectRoot is set; it reads only disk config). Nil is
	// safe — Candidates guards a nil receiver (#127).
	workspace *resolve.Workspace
}

// classFieldKey is the fieldTypes outer key. The NUL separator can't
// occur in a package path or identifier, so it never collides.
func classFieldKey(pkg, className string) string {
	return pkg + "\x00" + className
}

func (e *jstsBase) Init(_ context.Context, root string) error {
	e.projectRoot = root
	return nil
}

func (e *jstsBase) Finalize(_ context.Context) ([]Node, []Edge, []string, error) {
	// Build the workspace resolver now: projectRoot is set and it reads only
	// disk config (package.json / tsconfig), so it doesn't depend on knownFiles.
	e.workspace = resolve.Load(e.projectRoot)

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

	// Barrel re-exports were captured raw too; resolve their target specifiers to
	// packagePaths now, before resolveCall consults them (#127 Phase 2).
	e.resolveReExports()

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

	// Annotate import nodes with their resolution outcome so the package DAG
	// and dependency lanes see real cross-package targets instead of opaque
	// leaves. Node identity/display (the raw specifier) is unchanged (#127).
	e.annotateImports()

	return e.nodes, e.edges, e.warnings, nil
}

// annotateImports classifies each NodeImport's specifier and records the outcome
// in its metadata: Metadata["target"] = resolved internal module path, or
// Metadata["external"] = true for a bare npm dependency. An unresolved specifier
// is left untouched. The importing file is recovered via knownFiles[PackagePath].
func (e *jstsBase) annotateImports() {
	for i := range e.nodes {
		n := &e.nodes[i]
		if n.Kind != NodeImport {
			continue
		}
		fromFile := e.knownFiles[n.PackagePath]
		target, class, reason, pkgDir := e.classifySpecifier(n.QualifiedName, fromFile)
		if n.Metadata == nil {
			n.Metadata = map[string]any{}
		}
		switch class {
		case specInternal:
			n.Metadata["target"] = target
		case specExternal:
			n.Metadata["external"] = true
		case specUnresolved:
			// Explicit state — never a silent blank. reason (and pkg_dir for a
			// workspace subpath) let downstream tools surface the miss honestly
			// instead of guessing whether it's a bug, an external, or unprocessed.
			n.Metadata["unresolved"] = true
			if reason != "" {
				n.Metadata["reason"] = reason
			}
			if pkgDir != "" {
				n.Metadata["pkg_dir"] = pkgDir
			}
		}
	}
}

// emitFunctionNode emits the node/symbol/edge surface for a function or
// method (file→contains; class→has_method when className is set) WITHOUT
// descending into the body. Shared by the function/method emission
// and the query-driven tags path. Returns the node ID, or "" when the
// declaration has no usable name.
func (e *jstsBase) emitFunctionNode(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID, className string,
) string {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	name := nodeText(nameNode, src)
	if name == "" {
		return ""
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
		StartByte:     int(n.StartByte()),
		EndByte:       int(n.EndByte()),
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
	return id
}

// emitClassNode emits the class node, its symbol entry, and the
// file→contains edge WITHOUT descending into the body. Shared by the
// query-driven tags path's class emission. Returns the
// class name, or "" when the class has no usable name.
func (e *jstsBase) emitClassNode(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID string,
) string {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	name := nodeText(nameNode, src)
	if name == "" {
		return ""
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
		StartByte:     int(n.StartByte()),
		EndByte:       int(n.EndByte()),
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
	return name
}

// collectClassFieldTypes records the instance-field → type-name map for a
// class so `this.field.method()` can resolve DI/adapter dispatch name-based.
// It walks the class_body for the three shapes that give a field a known
// class type without a type checker:
//
//   - constructor parameter-properties — a `required_parameter` carrying an
//     accessibility_modifier / readonly (`constructor(private auth: Auth)`);
//   - typed field declarations (`private repo: UserRepo`);
//   - fields initialized by construction (`private cache = new Cache()`).
//
// Only a simple leading type_identifier is taken; unions, predefined types,
// and qualified/namespaced types are skipped (they'd resolve to nothing, not
// to a wrong edge). className is the emitted class node's name.
func (e *jstsBase) collectClassFieldTypes(classDecl *sitter.Node, src []byte, pkg, className string) {
	if className == "" {
		return
	}
	body := classDecl.ChildByFieldName("body")
	if body == nil {
		return
	}
	record := func(field, typeName string) {
		if field == "" || typeName == "" {
			return
		}
		key := classFieldKey(pkg, className)
		if e.fieldTypes[key] == nil {
			e.fieldTypes[key] = map[string]string{}
		}
		e.fieldTypes[key][field] = typeName
	}
	for i := 0; i < int(body.NamedChildCount()); i++ {
		member := body.NamedChild(i)
		if member == nil {
			continue
		}
		switch member.Type() {
		case "public_field_definition", "field_definition":
			name := nodeText(member.ChildByFieldName("name"), src)
			if t := member.ChildByFieldName("type"); t != nil {
				record(name, simpleTypeName(t, src))
			} else if v := member.ChildByFieldName("value"); v != nil && v.Type() == "new_expression" {
				record(name, nodeText(v.ChildByFieldName("constructor"), src))
			}
		case "method_definition":
			if nodeText(member.ChildByFieldName("name"), src) != "constructor" {
				continue
			}
			params := member.ChildByFieldName("parameters")
			if params == nil {
				continue
			}
			for j := 0; j < int(params.NamedChildCount()); j++ {
				p := params.NamedChild(j)
				if p == nil || p.Type() != "required_parameter" || !isParameterProperty(p) {
					continue
				}
				name := nodeText(p.ChildByFieldName("pattern"), src)
				if t := p.ChildByFieldName("type"); t != nil {
					record(name, simpleTypeName(t, src))
				}
			}
		}
	}
}

// isParameterProperty reports whether a constructor parameter is declared
// as an instance field — TypeScript promotes a parameter to a field when it
// carries an accessibility modifier (public/private/protected) or `readonly`.
func isParameterProperty(param *sitter.Node) bool {
	for i := 0; i < int(param.ChildCount()); i++ {
		switch c := param.Child(i); c.Type() {
		case "accessibility_modifier", "readonly", "override_modifier":
			return true
		}
	}
	return false
}

// simpleTypeName extracts the container class name from a type_annotation:
// `: Auth` → "Auth", `: Repository<User>` → "Repository" (method resolves on
// the container). Returns "" for unions, predefined types, qualified/generic
// bases we can't resolve name-based — the caller then emits no edge.
func simpleTypeName(typeAnnotation *sitter.Node, src []byte) string {
	// type_annotation wraps the actual type node; a bare type_identifier is
	// the resolvable case, and a generic_type's own type is its container.
	var t *sitter.Node
	if typeAnnotation.NamedChildCount() > 0 {
		t = typeAnnotation.NamedChild(0)
	}
	for t != nil {
		switch t.Type() {
		case "type_identifier":
			return nodeText(t, src)
		case "generic_type":
			t = t.ChildByFieldName("name")
		default:
			return ""
		}
	}
	return ""
}

// emitScaffold emits the package + file nodes and the package→file
// contains edge, returning the file node ID. Shared by the recursive
// query-driven tags path.
func (e *jstsBase) emitScaffold(in FileInput, pkg string) string {
	pkgID := NodeID("", pkg, NodePackage, pkg)
	e.addNode(Node{
		ID:            pkgID,
		Kind:          NodePackage,
		Name:          path.Base(pkg),
		QualifiedName: pkg,
		PackagePath:   pkg,
		Metadata:      map[string]any{"language": e.lang},
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
		Metadata:      map[string]any{"language": e.lang},
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
	return fileID
}

// addInterface emits a TypeScript interface as a node (no body recursion).
// Used for TypeScript interface declarations.
func (e *jstsBase) addInterface(
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
		StartByte:     int(n.StartByte()),
		EndByte:       int(n.EndByte()),
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
func (e *jstsBase) parseImportStatement(
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

	// Stash the "from" anchor for Finalize-time resolution.
	imports.modules["__from__"] = filePath

	for i := 0; i < int(n.NamedChildCount()); i++ {
		child := n.NamedChild(i)
		if child == nil || child == sourceNode {
			continue
		}
		if child.Type() != "import_clause" {
			continue
		}
		e.processImportClause(child, src, specifier, imports)
	}

	impID := NodeID("", currentPkg, NodeImport, specifier)
	e.addNode(Node{
		ID:            impID,
		Kind:          NodeImport,
		Name:          specifier,
		QualifiedName: specifier,
		PackagePath:   currentPkg,
		Metadata:      map[string]any{"language": e.lang},
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

// emitArrowDeclarator emits an arrow/function-expression const node for a
// single variable_declarator child of a lexical/variable declaration,
// WITHOUT collecting calls. n is the declaration statement (used for the
// node's start line). Returns the node ID and the
// function body (for the caller to walk), or ("", nil) when the
// declarator isn't a function-like binding. Shared by addLexicalDecl and
// the query-driven tags path.
func (e *jstsBase) emitArrowDeclarator(
	n, child *sitter.Node, src []byte,
	filePath, pkg, fileID string,
) (string, *sitter.Node) {
	if child == nil || child.Type() != "variable_declarator" {
		return "", nil
	}
	nameNode := child.ChildByFieldName("name")
	valueNode := child.ChildByFieldName("value")
	if nameNode == nil || valueNode == nil {
		return "", nil
	}
	name := nodeText(nameNode, src)
	if name == "" {
		return "", nil
	}
	// Only function-like values become function nodes.
	switch valueNode.Type() {
	case "arrow_function", "function", "function_expression":
	default:
		return "", nil
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
		StartByte:     int(n.StartByte()),
		EndByte:       int(n.EndByte()),
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
	return id, valueNode.ChildByFieldName("body")
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
			return e.resolveExport(fi.pkg, fi.name, map[string]bool{})
		}
		return ""

	case "self":
		if c.callerCls == "" {
			return ""
		}
		parts := c.calleeExpr.parts
		switch len(parts) {
		case 2:
			// this.method() — a method on the enclosing class.
			return e.symbolIn(c.callerPkg, c.callerCls+"."+parts[1])
		case 3:
			// this.field.method() — DI/adapter dispatch. Resolve the
			// field's declared/constructed type, then the method on it.
			typeName := e.fieldTypes[classFieldKey(c.callerPkg, c.callerCls)][parts[1]]
			if typeName == "" {
				return ""
			}
			return e.resolveTypeMethod(c.callerPkg, c.filePath, typeName, parts[2])
		}
		return ""

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
			// `import * as X from './barrel'; X.foo()` — foo may be re-exported.
			return e.resolveExport(mod, tail[0], map[string]bool{})
		}
		if fi, ok := imports.fromImports[head]; ok {
			if len(tail) != 1 {
				return ""
			}
			// `import { String } from '@acme/common'; String.capitalize()` where
			// the barrel binds String via `export * as String from './String'`.
			if nsMod := e.namespaceTarget(fi.pkg, fi.name); nsMod != "" {
				if id := e.resolveExport(nsMod, tail[0], map[string]bool{}); id != "" {
					return id
				}
			}
			return e.symbolIn(fi.pkg, fi.name+"."+tail[0])
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

// resolveTypeMethod resolves `<typeName>.<method>` to a method node, where
// typeName is a class named by a field-type annotation. It mirrors bare-call
// resolution: the class may be defined in the same package (file) or imported
// by name (`import { AuthService } from './auth'`). Returns "" when the type
// isn't a known project class or the method isn't on it.
func (e *jstsBase) resolveTypeMethod(pkg, filePath, typeName, method string) string {
	if id := e.symbolIn(pkg, typeName+"."+method); id != "" {
		return id
	}
	imports := e.fileImports[filePath]
	if imports == nil {
		return ""
	}
	if fi, ok := imports.fromImports[typeName]; ok {
		// The class may be re-exported through a barrel; star re-exports forward
		// the whole `Class.method` key, so resolveExport reaches it (#127 Phase 2).
		return e.resolveExport(fi.pkg, fi.name+"."+method, map[string]bool{})
	}
	return ""
}

// resolveModuleSpecifier turns a raw specifier (./foo, ../bar/baz,
// react) into the packagePath of the file it refers to, or returns
// the original string when the specifier doesn't resolve to a known
// project file. The dir+`/index` fallback covers barrel modules.
func (e *jstsBase) resolveModuleSpecifier(specifier, fromFile string) string {
	if specifier == "" {
		return ""
	}
	if strings.HasPrefix(specifier, ".") {
		if fromFile == "" {
			return specifier
		}
		base := path.Dir(filepath.ToSlash(fromFile))
		joined := path.Clean(path.Join(base, specifier))
		if resolved, ok := e.probeKnown(joined); ok {
			return resolved
		}
		return specifier
	}
	// Non-relative: a workspace package or tsconfig path alias may still name a
	// project file (@acme/*, @/*). A bare npm dep resolves nothing and is
	// returned verbatim — the caller treats it as external.
	if resolved, ok := e.resolveWorkspace(specifier); ok {
		return resolved
	}
	return specifier
}

// probeKnown resolves an extension-free, project-relative candidate module path
// against the indexed file set: exact hit, then explicit-extension strip (Deno /
// browser-native ESM), then the dir/index barrel fallback. Shared by relative
// and workspace resolution.
func (e *jstsBase) probeKnown(candidate string) (string, bool) {
	if _, ok := e.knownFiles[candidate]; ok {
		return candidate, true
	}
	if ext := path.Ext(candidate); ext != "" {
		stripped := candidate[:len(candidate)-len(ext)]
		if _, ok := e.knownFiles[stripped]; ok {
			return stripped, true
		}
	}
	if _, ok := e.knownFiles[candidate+"/index"]; ok {
		return candidate + "/index", true
	}
	return "", false
}

// resolveWorkspace probes the workspace resolver's candidates for a non-relative
// specifier against the indexed file set; the first indexed hit wins. Returns
// ("", false) when nothing resolves (a bare/external dependency).
func (e *jstsBase) resolveWorkspace(specifier string) (string, bool) {
	for _, cand := range e.workspace.Candidates(specifier) {
		if resolved, ok := e.probeKnown(cand); ok {
			return resolved, true
		}
	}
	return "", false
}

// specifierClass labels how an import specifier resolved, for import-node
// annotation.
type specifierClass int

const (
	specUnresolved specifierClass = iota // matched no indexed project file
	specInternal                         // resolved to an indexed project file
	specExternal                         // non-relative, no workspace/alias candidate (bare dep)
)

// classifySpecifier resolves a specifier and classifies the outcome for import-
// node metadata. Kept separate from resolveModuleSpecifier (which must keep
// returning a string for the call-resolution maps). For the unresolved class it
// also returns a reason (why it didn't resolve) and, for a workspace subpath,
// the matched package dir (pkgDir) — the join key that lets trace/impact
// attribute the miss to a package. reason/pkgDir are empty for internal/external.
func (e *jstsBase) classifySpecifier(specifier, fromFile string) (target string, class specifierClass, reason, pkgDir string) {
	if specifier == "" {
		return "", specUnresolved, "empty", ""
	}
	if strings.HasPrefix(specifier, ".") {
		if resolved := e.resolveModuleSpecifier(specifier, fromFile); resolved != specifier {
			return resolved, specInternal, "", ""
		}
		return "", specUnresolved, "relative", ""
	}
	if resolved, ok := e.resolveWorkspace(specifier); ok {
		return resolved, specInternal, "", ""
	}
	// Non-relative with no indexed target: external, unless the workspace knew a
	// candidate that simply wasn't indexed (then it's internal-but-unindexed).
	// The candidate's Origin tells us *why*, so the miss can be labeled honestly.
	c := e.workspace.Classify(specifier)
	if len(c.Candidates) > 0 {
		switch c.Origin {
		case resolve.OriginAlias:
			return "", specUnresolved, "alias-unindexed", ""
		case resolve.OriginWorkspace:
			return "", specUnresolved, "workspace-subpath", c.PkgDir
		default:
			return "", specUnresolved, "unresolved", ""
		}
	}
	return "", specExternal, "", ""
}

func (e *jstsBase) addNode(n Node) bool {
	if _, ok := e.nodeIDs[n.ID]; ok {
		return false
	}
	e.nodeIDs[n.ID] = struct{}{}
	e.nodes = append(e.nodes, n)
	return true
}
