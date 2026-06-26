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
		{"10", 10, 10, true}, // single line (#672)
		{"40-", 40, 0, true}, // open end: line 40 → EOF (#672)
		{"-10", 0, 10, true}, // open start: first 10 lines (#672)
		{"", 0, 0, false},
		{"40-10", 0, 0, false},   // end < start
		{"0-10", 0, 0, false},    // start < 1
		{"abc-10", 0, 0, false},  // non-numeric start
		{"10-abc", 0, 0, false},  // non-numeric end
		{"-", 0, 0, false},       // bare dash
		{"0", 0, 0, false},       // single line < 1
		{"5-10-15", 0, 0, false}, // multi-dash
		{"--5", 0, 0, false},     // negative end
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
	got := formatMap("server.go", syms, imports)
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
			t.Errorf("formatMap missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unexported") {
		t.Errorf("formatMap leaked unexported symbol; got:\n%s", got)
	}
}

func TestFormatSignatures(t *testing.T) {
	src := []byte("package main\n\nfunc Foo() {\n\treturn\n}\n\nfunc Bar(x int) int {\n\treturn x\n}\n")
	syms := []store.GraphSymbol{
		{Name: "Foo", QualifiedName: "Foo", Kind: "function", FilePath: "f.go", StartLine: 3, EndLine: 5},
		{Name: "Bar", QualifiedName: "Bar", Kind: "function", FilePath: "f.go", StartLine: 7, EndLine: 9},
	}
	got := formatSignatures(src, syms, "f.go", nil)
	for _, want := range []string{
		"f.go",
		"(2 symbols)",
		"⊛ Foo (lines 3-5)",
		"func Foo()",
		"⊛ Bar (lines 7-9)",
		"func Bar(x int) int {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatSignatures output missing %q\ngot:\n%s", want, got)
		}
	}
}

// TestSliceLinesOpenAndPastEOF covers #672: open-ended and past-EOF ranges must
// report the true last line (chunk.LineCount), not the walk's phantom-line
// count for trailing-newline files.
func TestSliceLinesOpenAndPastEOF(t *testing.T) {
	data := []byte("L1\nL2\nL3\nL4\nL5\nL6\n") // 6 content lines, trailing newline
	cases := []struct {
		name               string
		start, end         int
		wantContent        string
		wantStart, wantEnd int
	}{
		{"open end to EOF", 4, 0, "L4\nL5\nL6\n", 4, 6},
		{"open start first 2", 0, 2, "L1\nL2\n", 1, 2},
		{"past EOF clamps", 3, 100, "L3\nL4\nL5\nL6\n", 3, 6},
		{"single line", 5, 5, "L5\n", 5, 5},
		{"closed range", 3, 4, "L3\nL4\n", 3, 4},
		{"whole file", 0, 0, "L1\nL2\nL3\nL4\nL5\nL6\n", 1, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, s, e := SliceLines(data, c.start, c.end)
			if string(got) != c.wantContent || s != c.wantStart || e != c.wantEnd {
				t.Errorf("SliceLines(%d,%d) = (%q,%d,%d), want (%q,%d,%d)",
					c.start, c.end, got, s, e, c.wantContent, c.wantStart, c.wantEnd)
			}
		})
	}
}
