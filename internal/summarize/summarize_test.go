package summarize

import (
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func TestBuildSystem(t *testing.T) {
	base := BuildSystem("")
	for _, want := range []string{
		"file summarizer",  // file-kind agnostic, not "code summarizer"
		"Makefiles",        // hint that non-code files have their own framing
		"top-level keys",   // config files
		"section headings", // docs
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base prompt missing %q", want)
		}
	}
	if strings.Contains(base, "Focus specifically on") {
		t.Errorf("empty focus should not inject a focus clause; got: %s", base)
	}

	withFocus := BuildSystem("  public API surface  ")
	if !strings.Contains(withFocus, "Focus specifically on: public API surface.") {
		t.Errorf("focus clause missing or untrimmed; got: %s", withFocus)
	}
}

func TestParseLinesRange(t *testing.T) {
	tests := []struct {
		in        string
		wantStart int
		wantEnd   int
		wantOK    bool
	}{
		{"10-40", 10, 40, true},
		{"1-1", 1, 1, true},
		{"1-100", 1, 100, true},
		{"", 0, 0, false},
		{"10", 0, 0, false},
		{"40-10", 0, 0, false}, // end < start
		{"0-10", 0, 0, false},  // start < 1
		{"abc-10", 0, 0, false},
		{"10-abc", 0, 0, false},
		{"-10", 0, 0, false},
	}
	for _, tc := range tests {
		s, e, ok := ParseLinesRange(tc.in)
		if ok != tc.wantOK {
			t.Errorf("ParseLinesRange(%q): ok=%v want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && (s != tc.wantStart || e != tc.wantEnd) {
			t.Errorf("ParseLinesRange(%q): got %d-%d want %d-%d", tc.in, s, e, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestFormatMap(t *testing.T) {
	syms := []store.GraphSymbol{
		{Name: "Server", QualifiedName: "mcp.Server", Kind: "struct", FilePath: "server.go", StartLine: 10, EndLine: 50},
		{Name: "unexported", QualifiedName: "mcp.unexported", Kind: "struct", FilePath: "server.go", StartLine: 55, EndLine: 60},
		{Name: "Run", QualifiedName: "mcp.Server.Run", Kind: "method", FilePath: "server.go", StartLine: 100, EndLine: 120},
	}
	imports := []string{"context", "fmt", "os"}
	got := FormatMap("server.go", syms, imports)
	for _, want := range []string{
		"FILE: server.go",
		"IMPORTS:",
		"  context",
		"  fmt",
		"  os",
		"EXPORTS (2):",
		"struct mcp.Server",
		"method mcp.Server.Run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatMap missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unexported") {
		t.Errorf("FormatMap leaked unexported symbol; got:\n%s", got)
	}
}

func TestFormatSignatures(t *testing.T) {
	src := []byte("package main\n\nfunc Foo() {\n\treturn\n}\n\nfunc Bar(x int) int {\n\treturn x\n}\n")
	syms := []store.GraphSymbol{
		{Name: "Foo", QualifiedName: "Foo", Kind: "function", FilePath: "f.go", StartLine: 3, EndLine: 5},
		{Name: "Bar", QualifiedName: "Bar", Kind: "function", FilePath: "f.go", StartLine: 7, EndLine: 9},
	}
	got := FormatSignatures(src, syms, "f.go", nil)
	for _, want := range []string{
		"f.go",
		"(2 symbols)",
		"⊛ Foo (lines 3-5)",
		"func Foo()",
		"⊛ Bar (lines 7-9)",
		"func Bar(x int) int {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatSignatures output missing %q\ngot:\n%s", want, got)
		}
	}
}
