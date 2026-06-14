package graph

import (
	"context"
	"fmt"
	"sort"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
)

// Query-driven (tags) extractor for Java. It replaces the recursive
// descent of the java walker with one tree-sitter query that enumerates
// every type / method / constructor / import / call; scope is recovered
// by walking each match's ancestors. The resolution layer (parseImport /
// resolveCall / Finalize, all on javaExtractor) is reused verbatim, so
// the graph is identical to the walker's.
//
// The walker models only top-level types and their direct members —
// nested and local types, and the bodies of anonymous classes' methods,
// are not graph nodes. The ancestor walks below reproduce exactly that
// reachability so the two front-ends agree node-for-node and edge-for-edge.

const javaTagsQuery = `
(class_declaration) @class
(record_declaration) @record
(interface_declaration) @interface
(enum_declaration) @enum
(method_declaration) @method
(constructor_declaration) @ctor
(import_declaration) @import
(method_invocation) @call
(object_creation_expression) @call
`

// javaTagsExtractor is the query-driven counterpart to javaExtractor. It
// embeds *javaExtractor so Init / Finalize / resolveCall / the emit
// helpers / import parsing are inherited unchanged.
type javaTagsExtractor struct {
	*javaExtractor
	query *sitter.Query
}

func newJavaTagsExtractor() Extractor {
	return &javaTagsExtractor{javaExtractor: newJavaExtractorImpl()}
}

func (e *javaTagsExtractor) Name() string               { return "java-tags" }
func (e *javaTagsExtractor) Language() *sitter.Language { return java.GetLanguage() }
func (e *javaTagsExtractor) Extensions() []string       { return []string{".java"} }

func (e *javaTagsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg := javaPackagePath(in.Root, in.Source, in.RelPath)
	fileID := e.emitScaffold(in, pkg)

	imports := &javaImportTable{
		classes: map[string]javaClassImport{},
		statics: map[string]javaStaticImport{},
	}
	e.fileImports[in.RelPath] = imports

	if e.query == nil {
		q, err := sitter.NewQuery([]byte(javaTagsQuery), e.Language())
		if err != nil {
			return fmt.Errorf("compile java tags query: %w", err)
		}
		e.query = q
	}

	var classes, records, ifaces, enums, methods, ctors, imps, calls []*sitter.Node
	runTagsQuery(e.query, in.Root, func(capture string, n *sitter.Node) {
		switch capture {
		case "class":
			classes = append(classes, n)
		case "record":
			records = append(records, n)
		case "interface":
			ifaces = append(ifaces, n)
		case "enum":
			enums = append(enums, n)
		case "method":
			methods = append(methods, n)
		case "ctor":
			ctors = append(ctors, n)
		case "import":
			imps = append(imps, n)
		case "call":
			calls = append(calls, n)
		}
	})

	// Imports — top-level only (direct children of the root), matching the
	// walker's processTopLevel.
	for _, n := range imps {
		if !javaTopLevel(n) {
			continue
		}
		e.parseImport(n, in.Source, in.RelPath, pkg, fileID, imports)
	}

	// Top-level types. class / record / enum are NodeClass; interface is
	// NodeInterface — exactly the processTopLevel mapping.
	for _, n := range classes {
		if javaTopLevel(n) {
			e.emitClassLikeNode(n, in.Source, in.RelPath, pkg, fileID, NodeClass)
		}
	}
	for _, n := range records {
		if javaTopLevel(n) {
			e.emitClassLikeNode(n, in.Source, in.RelPath, pkg, fileID, NodeClass)
		}
	}
	for _, n := range enums {
		if javaTopLevel(n) {
			e.emitClassLikeNode(n, in.Source, in.RelPath, pkg, fileID, NodeClass)
		}
	}
	for _, n := range ifaces {
		if javaTopLevel(n) {
			e.emitClassLikeNode(n, in.Source, in.RelPath, pkg, fileID, NodeInterface)
		}
	}

	// Methods and constructors — direct members of a top-level type. Emit in
	// document order so the "first overload wins" bare-name registration in
	// emitMethodNode matches the walker's per-class source-order traversal.
	members := append(append([]*sitter.Node{}, methods...), ctors...)
	sort.SliceStable(members, func(i, j int) bool {
		return members[i].StartByte() < members[j].StartByte()
	})
	for _, n := range members {
		className, ok := e.javaMethodClass(n, in.Source)
		if !ok {
			continue
		}
		e.emitMethodNode(n, in.Source, in.RelPath, pkg, fileID, className, n.Type() == "constructor_declaration")
	}

	// Calls — attributed to the enclosing method's body, mirroring
	// collectCalls (stop at the nearest type/method/lambda boundary;
	// top-level-type methods only; body subtree only).
	for _, n := range calls {
		e.collectQueryCall(n, in.Source, in.RelPath, pkg)
	}
	return nil
}

// collectQueryCall replicates collectCalls' attribution for a single call
// (method_invocation / object_creation_expression) discovered by the query.
func (e *javaTagsExtractor) collectQueryCall(n *sitter.Node, src []byte, filePath, pkg string) {
	// Nearest enclosing scope boundary — the same node types that make
	// collectCalls' descent return. record_declaration is deliberately
	// absent: collectCalls treats it (and anonymous-class class_body) as
	// transparent, so calls in their field initializers attribute to the
	// enclosing method.
	boundary := firstAncestorOfType(n,
		"class_declaration", "interface_declaration", "enum_declaration",
		"method_declaration", "constructor_declaration", "lambda_expression")
	if boundary == nil {
		return
	}
	isCtor := boundary.Type() == "constructor_declaration"
	if boundary.Type() != "method_declaration" && !isCtor {
		return
	}
	className, ok := e.javaMethodClass(boundary, src)
	if !ok {
		return
	}
	// The call must sit in the method's body, not its signature/annotations.
	if body := boundary.ChildByFieldName("body"); !nodeContains(body, n) {
		return
	}

	methodName := "<init>"
	if !isCtor {
		methodName = nodeText(boundary.ChildByFieldName("name"), src)
		if methodName == "" {
			return
		}
	}
	qn := className + "." + methodName + "(" + javaParamSig(boundary, src) + ")"
	callerID := NodeID("", pkg, NodeMethod, qn)

	var expr javaCallee
	switch n.Type() {
	case "method_invocation":
		expr = classifyJavaInvocation(n, src)
	case "object_creation_expression":
		expr = classifyJavaNewExpr(n, src)
	}
	if expr.kind == "skip" {
		return
	}
	e.pendingCalls = append(e.pendingCalls, javaPendingCall{
		callerID:   callerID,
		callerPkg:  pkg,
		callerCls:  className,
		calleeExpr: expr,
		filePath:   filePath,
		line:       lineOfPoint(n.StartPoint().Row),
	})
}

// javaTopLevel reports whether n is a direct child of the tree root — the
// definition of "top-level" the walker uses by iterating in.Root.NamedChild.
// The root is normally a program node, but on a malformed parse it can be an
// ERROR node whose children are the still-valid leading declarations; the
// walker captures those, so the tags path must too.
func javaTopLevel(n *sitter.Node) bool {
	return isTreeRoot(n.Parent())
}

// javaMethodClass returns the enclosing class name for a method or
// constructor that is a direct member of a TOP-LEVEL type, and ok=false
// otherwise. Members nested in local/anonymous types, or inside an enum's
// enum_body_declarations, fail the body-type or top-level checks — exactly
// the declarations the walker's per-class body loop skips.
func (e *javaTagsExtractor) javaMethodClass(method *sitter.Node, src []byte) (string, bool) {
	body := method.Parent()
	if body == nil {
		return "", false
	}
	// class_body covers class + record; interface_body covers interfaces.
	// enum methods live in enum_body_declarations, which the walker never
	// descends — excluded here by not matching either body type.
	if t := body.Type(); t != "class_body" && t != "interface_body" {
		return "", false
	}
	typeDecl := body.Parent()
	if typeDecl == nil {
		return "", false
	}
	switch typeDecl.Type() {
	case "class_declaration", "record_declaration", "interface_declaration":
	default:
		return "", false
	}
	if !javaTopLevel(typeDecl) {
		return "", false
	}
	name := nodeText(typeDecl.ChildByFieldName("name"), src)
	if name == "" {
		return "", false
	}
	return name, true
}
