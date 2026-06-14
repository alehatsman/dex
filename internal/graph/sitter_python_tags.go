package graph

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
)

// pythonTagsQuery enumerates every definition, call, and import in one
// tree-sitter pass. It replaces the recursive descent (processTopLevel /
// addFunction / addClass / collectCalls); scope — method-of-class,
// caller-of-call, nesting — is recovered afterwards by walking each
// match's ancestors. This is the 468b pilot: discovery moves into a
// query while the resolution layer (import table / symbol table /
// resolveCall, via the embedded pythonExtractor) is reused verbatim.
const pythonTagsQuery = `
(function_definition) @function
(class_definition) @class
(call) @call
(import_statement) @import
(import_from_statement) @import_from
`

// pythonTagsExtractor is the query-driven counterpart to pythonExtractor.
// It embeds pythonExtractor so Init / Finalize / resolveCall / addNode /
// emit helpers / import parsers are inherited unchanged — only discovery
// (ProcessFile) is replaced. Node identity is content-addressed, so a
// correct discovery pass yields a byte-identical graph and an identical
// trace score; the win is "one query replaces a walker", not precision.
type pythonTagsExtractor struct {
	*pythonExtractor
	query *sitter.Query
}

func newPythonTagsExtractor() Extractor {
	return &pythonTagsExtractor{pythonExtractor: newPythonExtractorImpl()}
}

// Name is distinct from the walker so the index's sitter_lang provenance
// records which discovery front-end built the graph (A/B attribution).
// Node/edge IDs are unaffected — they key off symbol names, not this.
func (e *pythonTagsExtractor) Name() string { return "python-tags" }

func (e *pythonTagsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg, fileID, imports := e.emitFileScaffold(in)

	if e.query == nil {
		q, err := sitter.NewQuery([]byte(pythonTagsQuery), e.Language())
		if err != nil {
			return fmt.Errorf("compile python tags query: %w", err)
		}
		e.query = q
	}

	// Bucket every capture by kind. Query traversal is whole-tree, so
	// these lists are the flat equivalent of what the recursive walker
	// would have visited — scope is reconstructed below.
	var funcs, classes, calls, imps []*sitter.Node
	qc := sitter.NewQueryCursor()
	qc.Exec(e.query, in.Root)
	for {
		m, ok := qc.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			switch e.query.CaptureNameForId(c.Index) {
			case "function":
				funcs = append(funcs, c.Node)
			case "class":
				classes = append(classes, c.Node)
			case "call":
				calls = append(calls, c.Node)
			case "import", "import_from":
				imps = append(imps, c.Node)
			}
		}
	}
	qc.Close()

	// Imports — only direct children of the module node. The walker
	// processes top-level imports exclusively; imports inside if/try
	// blocks or function bodies are intentionally not indexed.
	for _, n := range imps {
		p := n.Parent()
		if p == nil || p.Type() != "module" {
			continue
		}
		switch n.Type() {
		case "import_statement":
			e.parseImport(n, in.Source, in.RelPath, pkg, fileID, imports)
		case "import_from_statement":
			e.parseImportFrom(n, in.Source, in.RelPath, pkg, fileID, imports)
		}
	}

	// Classes — emitted unless nested inside a function. The walker only
	// descends module → class → class, so a class defined inside a
	// function body is unreachable and never becomes a node.
	for _, n := range classes {
		if pyHasFunctionAncestor(n) {
			continue
		}
		e.emitClassNode(n, in.Source, in.RelPath, pkg, fileID)
	}

	// Functions / methods — same reachability rule. className is the
	// nearest enclosing class (a method), else "" (a top-level func).
	for _, n := range funcs {
		if pyHasFunctionAncestor(n) {
			continue
		}
		className := ""
		if cls := pyNearestClassAncestor(n); cls != nil {
			className = nodeText(cls.ChildByFieldName("name"), in.Source)
		}
		e.emitFunctionNode(n, in.Source, in.RelPath, pkg, fileID, className)
	}

	// Calls — attributed to the enclosing node-function's body, mirroring
	// collectCalls (stop at nested def / class / lambda; method bodies
	// only; body subtree only, not params/decorators/defaults).
	for _, n := range calls {
		e.collectQueryCall(n, in.Source, pkg, in.RelPath)
	}
	return nil
}

// collectQueryCall replicates collectCalls' attribution for a single call
// node discovered by the query. It records a pending call only when the
// call sits directly in the body of a reachable function/method, with no
// intervening nested def, class, or lambda — exactly the set the
// recursive walker would have collected.
func (e *pythonTagsExtractor) collectQueryCall(n *sitter.Node, src []byte, pkg, filePath string) {
	// Nearest enclosing scope boundary. A lambda or class boundary
	// reached before any function means the walker never collected this
	// call (it stops at lambdas and only walks method bodies).
	var fn *sitter.Node
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "function_definition":
			fn = p
		case "class_definition", "lambda":
			return
		}
		if fn != nil {
			break
		}
	}
	if fn == nil {
		return // module-level call — never collected
	}
	// A function nested in another function is not a node, so the walker
	// never ran collectCalls over its body.
	if pyHasFunctionAncestor(fn) {
		return
	}
	// The call must live in the function's body, not its parameter list,
	// decorators, or default-argument expressions.
	body := fn.ChildByFieldName("body")
	if body == nil || n.StartByte() < body.StartByte() || n.StartByte() >= body.EndByte() {
		return
	}

	name := nodeText(fn.ChildByFieldName("name"), src)
	if name == "" {
		return
	}
	className := ""
	if cls := pyNearestClassAncestor(fn); cls != nil {
		className = nodeText(cls.ChildByFieldName("name"), src)
	}
	kind := NodeFunction
	qn := name
	if className != "" {
		kind = NodeMethod
		qn = className + "." + name
	}

	callee := n.ChildByFieldName("function")
	if callee == nil {
		return
	}
	expr := classifyCallee(callee, src)
	if expr.kind == "skip" {
		return
	}
	e.pendingCalls = append(e.pendingCalls, pyPendingCall{
		callerID:   NodeID("", pkg, kind, qn),
		callerPkg:  pkg,
		callerCls:  className,
		calleeExpr: expr,
		filePath:   filePath,
		line:       lineOfPoint(n.StartPoint().Row),
	})
}

// pyHasFunctionAncestor reports whether n is nested inside any
// function_definition — making it unreachable by the recursive walker,
// which never descends into nested defs.
func pyHasFunctionAncestor(n *sitter.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() == "function_definition" {
			return true
		}
	}
	return false
}

// pyNearestClassAncestor returns the closest enclosing class_definition,
// stopping (returning nil) at the first function boundary — used to
// decide whether a function is a method and under which class.
func pyNearestClassAncestor(n *sitter.Node) *sitter.Node {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "function_definition":
			return nil
		case "class_definition":
			return p
		}
	}
	return nil
}
