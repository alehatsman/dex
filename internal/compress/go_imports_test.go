package compress

import (
	"strings"
	"testing"
)

// fourFilePackage returns four Go source files that share common imports and
// each have some unique ones — the fixture used by the acceptance tests.
func fourFilePackage() []string {
	return []string{
		// server.go
		`package mcp

import (
	"context"
	"fmt"
	"os"
)

func NewServer() {}`,

		// handler.go
		`package mcp

import (
	"context"
	"errors"
	"fmt"
)

func Handle() {}`,

		// client.go
		`package mcp

import (
	"context"
	"fmt"
	"net/http"
)

func Dial() {}`,

		// util.go
		`package mcp

import (
	"fmt"
	"strings"
)

func Format() {}`,
	}
}

func TestExtractGoImportBlock_Basic(t *testing.T) {
	src := `package foo

import (
	"context"
	"fmt"
)

func Foo() {}`

	before, after, paths, blockLines, ok := ExtractGoImportBlock(src)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(before, "package foo") {
		t.Errorf("before should contain package line, got %q", before)
	}
	if !strings.Contains(after, "func Foo") {
		t.Errorf("after should contain func, got %q", after)
	}
	if len(paths) != 2 || paths[0] != "context" || paths[1] != "fmt" {
		t.Errorf("unexpected paths: %v", paths)
	}
	// "import (" + 2 paths + ")" = 4 lines
	if blockLines != 4 {
		t.Errorf("blockLines: got %d, want 4", blockLines)
	}
}

func TestExtractGoImportBlock_NoBlock(t *testing.T) {
	src := `package foo

import "fmt"

func Foo() {}`

	_, _, _, _, ok := ExtractGoImportBlock(src)
	if ok {
		t.Error("single-import form should return ok=false")
	}
}

func TestExtractGoImportBlock_AliasedImport(t *testing.T) {
	src := `package foo

import (
	myfmt "fmt"
	"os"
)

func Foo() {}`

	_, _, paths, _, ok := ExtractGoImportBlock(src)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %v", paths)
	}
	// Aliased import: path should be "fmt", not "myfmt"
	if paths[0] != "fmt" || paths[1] != "os" {
		t.Errorf("expected [fmt os], got %v", paths)
	}
}

func TestDeduplicateGoImports_FourFiles(t *testing.T) {
	files := fourFilePackage()
	header, deduped, savedLines := DeduplicateGoImports(files)

	// Shared header must be non-empty and contain the union of all imports.
	if header == "" {
		t.Fatal("expected non-empty shared header")
	}
	for _, imp := range []string{"context", "errors", "fmt", "net/http", "os", "strings"} {
		if !strings.Contains(header, imp) {
			t.Errorf("shared header missing import %q", imp)
		}
	}
	if !strings.HasPrefix(header, "// Shared imports (4 files):") {
		t.Errorf("unexpected header prefix: %q", header)
	}

	// Each file's content must have the back-reference and no original import block.
	for i, d := range deduped {
		if !strings.Contains(d, "// imports: see shared header above") {
			t.Errorf("file %d: missing back-reference", i)
		}
		if strings.Contains(d, "import (") {
			t.Errorf("file %d: original import block not removed", i)
		}
	}

	// Per-file function declarations must survive.
	for _, want := range []string{"NewServer", "Handle", "Dial", "Format"} {
		found := false
		for _, d := range deduped {
			if strings.Contains(d, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("function %q not found in any deduped file", want)
		}
	}

	// savedLines must be positive (we removed at least the 4-line blocks from 4 files).
	if savedLines <= 0 {
		t.Errorf("savedLines should be > 0, got %d", savedLines)
	}
}

func TestDeduplicateGoImports_SingleFile(t *testing.T) {
	files := fourFilePackage()[:1]
	header, deduped, savedLines := DeduplicateGoImports(files)
	if header != "" {
		t.Error("single file: expected empty header (dedup requires ≥2 files)")
	}
	if deduped[0] != files[0] {
		t.Error("single file: content should be unchanged")
	}
	if savedLines != 0 {
		t.Errorf("single file: savedLines should be 0, got %d", savedLines)
	}
}

func TestDeduplicateGoImports_NonGoFiles(t *testing.T) {
	// Files with no parenthesized import block (e.g. non-Go or single-import form).
	files := []string{
		"package foo\nimport \"fmt\"\nfunc A() {}",
		"package foo\nimport \"os\"\nfunc B() {}",
		"package foo\nimport \"io\"\nfunc C() {}",
	}
	header, deduped, savedLines := DeduplicateGoImports(files)
	if header != "" {
		t.Error("no parenthesized blocks: expected empty header")
	}
	for i, d := range deduped {
		if d != files[i] {
			t.Errorf("file %d: content changed unexpectedly", i)
		}
	}
	if savedLines != 0 {
		t.Errorf("savedLines should be 0, got %d", savedLines)
	}
}

func TestDeduplicateGoImports_MixedFiles(t *testing.T) {
	// 3 Go files + 1 non-Go (no import block): dedup still applies to the 3.
	files := []string{
		"package foo\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n\nfunc A() {}",
		"package foo\n\nimport (\n\t\"context\"\n\t\"os\"\n)\n\nfunc B() {}",
		"# not Go\nsome random text\n",
	}
	header, deduped, _ := DeduplicateGoImports(files)
	if header == "" {
		t.Fatal("expected shared header for 2+ files with import blocks")
	}
	// Non-Go file must be unchanged.
	if deduped[2] != files[2] {
		t.Error("non-Go file should be unchanged")
	}
	// Go files must have back-references.
	for _, i := range []int{0, 1} {
		if !strings.Contains(deduped[i], "// imports: see shared header above") {
			t.Errorf("file %d: missing back-reference", i)
		}
	}
}
