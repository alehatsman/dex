package graph

// JavaScript-specific helpers for the tree-sitter extractor. The
// discovery logic lives in the query-driven extractor (sitter_jsts_tags.go);
// resolution and emit state live in jstsBase (sitter_jsts.go). What remains
// here is the JS package-path rule and the shared base constructor.
//
// Package-path scheme: file path minus extension, slash form. Resolution
// lanes (bare, this.X, namespace member, named/default import, new
// ClassName) and specifier probing (.js, .jsx, /index.js) are handled in
// jstsBase via the extension-stripped packagePath.
//
// Cross-language calls (JS → TS or TS → JS) don't resolve: each extractor
// instance owns its own symbol table. Pure JS or pure TS projects are the
// common case; mixed projects fall back to file + import edges without
// callee linkage.

import (
	"path/filepath"
	"strings"
)

// newJSTSBase builds the shared accumulator/resolution state for a
// JavaScript or TypeScript tags extractor.
func newJSTSBase(lang string) jstsBase {
	return jstsBase{
		lang:        lang,
		nodeIDs:     map[string]struct{}{},
		symbols:     map[string]map[string]string{},
		fileImports: map[string]*tsImportTable{},
		knownFiles:  map[string]string{},
		fieldTypes:  map[string]map[string]string{},
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
