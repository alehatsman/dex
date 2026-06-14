package graph

// JavaScript tree-sitter extractor. Handles .js and .jsx via the
// javascript grammar (mirrors internal/chunk/chunk.go's choice).
//
// Structurally a port of the TypeScript extractor (sitter_ts.go)
// minus the type-only declarations (interface, type alias) the JS
// grammar doesn't surface. Same package-path scheme (file path minus
// extension, slash form), same resolution lanes (bare, this.X,
// namespace member, named/default import, new ClassName), and same
// specifier-resolution probing (.js, .jsx, /index.js) — done via
// extension-stripped packagePath so the lookup is one map check.
//
// Cross-language calls (JS → TS or TS → JS) don't resolve: each
// extractor instance owns its own symbol table. Pure JS or pure TS
// projects are the common case; mixed projects fall back to file
// edges + import edges without callee linkage.
//
// Shared state and methods live in jstsBase (sitter_jsts.go).

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/javascript"
)

func newJSExtractor() Extractor {
	return &jsExtractor{jstsBase: newJSTSBase("javascript")}
}

// newJSTSBase builds the shared accumulator/resolution state for a
// JavaScript or TypeScript extractor (walker or tags-driven).
func newJSTSBase(lang string) jstsBase {
	return jstsBase{
		lang:        lang,
		nodeIDs:     map[string]struct{}{},
		symbols:     map[string]map[string]string{},
		fileImports: map[string]*tsImportTable{},
		knownFiles:  map[string]string{},
	}
}

type jsExtractor struct {
	jstsBase
}

// ---- Extractor interface ---------------------------------------------------

func (e *jsExtractor) Name() string               { return "javascript" }
func (e *jsExtractor) Language() *sitter.Language { return javascript.GetLanguage() }
func (e *jsExtractor) Extensions() []string       { return []string{".js", ".jsx"} }

func (e *jsExtractor) ProcessFile(_ context.Context, in FileInput) error {
	pkg := jsPackagePath(in.RelPath)
	e.knownFiles[pkg] = in.RelPath

	pkgID := NodeID("", pkg, NodePackage, pkg)
	e.addNode(Node{
		ID:            pkgID,
		Kind:          NodePackage,
		Name:          path.Base(pkg),
		QualifiedName: pkg,
		PackagePath:   pkg,
		Metadata:      map[string]any{"language": "javascript"},
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
		Metadata:      map[string]any{"language": "javascript"},
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

func (e *jsExtractor) processTopLevel(
	n *sitter.Node, src []byte,
	filePath, pkg, fileID string, imports *tsImportTable,
) {
	if n == nil {
		return
	}
	kind := n.Type()
	if kind == "export_statement" {
		if decl := n.ChildByFieldName("declaration"); decl != nil {
			e.processTopLevel(decl, src, filePath, pkg, fileID, imports)
			e.maybeMarkDefaultExport(decl, src, pkg)
			return
		}
		return
	}
	switch kind {
	case "function_declaration":
		e.addFunction(n, src, filePath, pkg, fileID, "")
	case "class_declaration":
		e.addClass(n, src, filePath, pkg, fileID)
	case "lexical_declaration", "variable_declaration":
		e.addLexicalDecl(n, src, filePath, pkg, fileID)
	case "import_statement":
		e.parseImportStatement(n, src, filePath, pkg, fileID, imports)
	}
}

// jsPackagePath strips the trailing `.js` or `.jsx` extension. Files
// without those extensions are returned verbatim so the caller still
// gets a stable key.
func jsPackagePath(relPath string) string {
	p := filepath.ToSlash(relPath)
	for _, ext := range []string{".jsx", ".js"} {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext)
		}
	}
	return p
}
