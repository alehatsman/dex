package main

import (
	"go/parser"
	"go/token"
	"testing"
)

// queryVerbFiles are the CLI files implementing the query verb (#849 CLI
// collapse) — the front door for every use-case-1-through-6 retrieval lane
// (read/grep/locate/symbol/search/assemble/review/orient/check/refs/cohort/
// deps/status). Per the spec's Validation section, none of them may import a
// retrieval internal directly; they must go through internal/mcp only, so the
// CLI can never grow a second classifier/reimplementation the way the old
// `ask`/`search` verbs did. Add a new file here if the query verb grows one.
var queryVerbFiles = []string{
	"query.go",
	"query_render.go",
}

// forbiddenRetrievalImports are the internals a query-verb file must reach
// only through internal/mcp, never directly.
var forbiddenRetrievalImports = []string{
	"github.com/alehatsman/dex/internal/embed",
	"github.com/alehatsman/dex/internal/store",
	"github.com/alehatsman/dex/internal/graph",
}

// TestQueryVerbFilesOnlyImportMCP guards the #849 spec's Validation bullet:
// "no cmd/dex/*.go file implementing a use-case-1-through-6 verb may import
// internal/embed, internal/store, or internal/graph directly — only
// internal/mcp." A regression here is exactly the drift that made `ask` and
// `search` two competing classifiers before the collapse.
func TestQueryVerbFilesOnlyImportMCP(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range queryVerbFiles {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := imp.Path.Value // includes surrounding quotes
			for _, forbidden := range forbiddenRetrievalImports {
				if path == `"`+forbidden+`"` {
					t.Errorf("%s imports %s directly — retrieval must go through internal/mcp only", name, forbidden)
				}
			}
		}
	}
}
