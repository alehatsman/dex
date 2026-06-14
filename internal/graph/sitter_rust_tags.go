package graph

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/rust"
)

// Query-driven (tags) extractor for Rust. It replaces the recursive
// one tree-sitter query that enumerates
// every item; scope is recovered by walking each match's ancestors. The
// resolution layer (parseUseDecl / resolveCall / Finalize) and the emit
// units (addFunction / addType, which include call collection) are reused
// verbatim via an embedded *rustExtractor, so the graph is identical.
//
// Only top-level items are modelled, and inline modules
// (`mod foo { ... }`) are NOT descended — items inside them are invisible.
// The ancestor walks below enforce that reachability:
//   - free functions      — function_item whose parent is the tree root
//   - impl / trait methods — function_item in the declaration_list body of
//     a TOP-LEVEL impl_item / trait_item (receiver = impl type / trait name)
//   - struct / enum / trait / use / `mod foo;` — top-level only
// Call attribution rides on addFunction's own call collection.

const rustTagsQuery = `
(function_item) @function
(struct_item) @struct
(enum_item) @enum
(trait_item) @trait
(impl_item) @impl
(use_declaration) @use
(mod_item) @mod
`

// rustTagsExtractor is the query-driven counterpart to rustExtractor.
type rustTagsExtractor struct {
	*rustExtractor
	query *sitter.Query
}

func newRustTagsExtractor() Extractor {
	return &rustTagsExtractor{rustExtractor: newRustExtractorImpl()}
}

func (e *rustTagsExtractor) Name() string               { return "rust" }
func (e *rustTagsExtractor) Language() *sitter.Language { return rust.GetLanguage() }
func (e *rustTagsExtractor) Extensions() []string       { return []string{".rs"} }

func (e *rustTagsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg := rustPackagePath(in.RelPath)
	fileID := e.emitScaffold(in, pkg)

	imports := &rustImportTable{fromImports: map[string]rustImport{}}
	e.fileImports[in.RelPath] = imports

	if e.query == nil {
		q, err := sitter.NewQuery([]byte(rustTagsQuery), e.Language())
		if err != nil {
			return fmt.Errorf("compile rust tags query: %w", err)
		}
		e.query = q
	}

	var funcs, structs, enums, traits, impls, uses, mods []*sitter.Node
	runTagsQuery(e.query, in.Root, func(capture string, n *sitter.Node) {
		switch capture {
		case "function":
			funcs = append(funcs, n)
		case "struct":
			structs = append(structs, n)
		case "enum":
			enums = append(enums, n)
		case "trait":
			traits = append(traits, n)
		case "impl":
			impls = append(impls, n)
		case "use":
			uses = append(uses, n)
		case "mod":
			mods = append(mods, n)
		}
	})

	// Imports — top-level use_declarations and bare `mod foo;` decls.
	for _, n := range uses {
		if rustTopLevel(n) {
			e.parseUseDecl(n, in.Source, in.RelPath, pkg, fileID, imports)
		}
	}
	for _, n := range mods {
		if rustTopLevel(n) && n.ChildByFieldName("body") == nil {
			e.emitModDecl(n, in.Source, in.RelPath, pkg, fileID)
		}
	}

	// Top-level types and traits. struct/enum are NodeClass; the trait NODE
	// is emitted here, its methods come through the function bucket.
	for _, n := range structs {
		if rustTopLevel(n) {
			e.addType(n, in.Source, in.RelPath, pkg, fileID, NodeClass)
		}
	}
	for _, n := range enums {
		if rustTopLevel(n) {
			e.addType(n, in.Source, in.RelPath, pkg, fileID, NodeClass)
		}
	}
	for _, n := range traits {
		if rustTopLevel(n) {
			e.emitTraitNode(n, in.Source, in.RelPath, pkg, fileID)
		}
	}

	// Top-level impls — emit the implements edge only; members ride the
	// function bucket.
	for _, n := range impls {
		if rustTopLevel(n) {
			e.emitImplEdge(n, in.Source, pkg)
		}
	}

	// Functions — free fns and impl/trait methods. addFunction emits the
	// node, the contains/has_method edges, and collects the body's calls.
	for _, n := range funcs {
		recv, ok := e.rustFnReceiver(n, in.Source)
		if !ok {
			continue
		}
		e.addFunction(n, in.Source, in.RelPath, pkg, fileID, recv)
	}
	return nil
}

// rustTopLevel reports whether n is a direct child of the tree root — the
// "top-level" = direct child of the tree root.
func rustTopLevel(n *sitter.Node) bool {
	return isTreeRoot(n.Parent())
}

// rustFnReceiver returns the receiver type for a function_item and whether
// it is a graph node at all. A function is a node iff it is a free function
// (direct child of the root) or a direct member of a TOP-LEVEL impl/trait
// body. Functions nested in other functions, closures, or inline modules
// are not nodes.
func (e *rustTagsExtractor) rustFnReceiver(fn *sitter.Node, src []byte) (string, bool) {
	parent := fn.Parent()
	if parent == nil {
		return "", false
	}
	if isTreeRoot(parent) {
		return "", true // free function
	}
	if parent.Type() != "declaration_list" {
		return "", false
	}
	owner := parent.Parent()
	if owner == nil || !rustTopLevel(owner) {
		return "", false
	}
	switch owner.Type() {
	case "impl_item":
		typeNode := owner.ChildByFieldName("type")
		if typeNode == nil {
			return "", false
		}
		recv := rustTypeName(typeNode, src)
		return recv, recv != ""
	case "trait_item":
		nameNode := owner.ChildByFieldName("name")
		if nameNode == nil {
			return "", false
		}
		recv := nodeText(nameNode, src)
		return recv, recv != ""
	}
	return "", false
}
