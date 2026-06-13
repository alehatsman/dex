package compress

import (
	"strings"
	"testing"
)

func TestBuildCodebook_MinFiles(t *testing.T) {
	files := []string{
		"import \"fmt\"\nfmt.Println()",
		"import \"fmt\"\nfmt.Println()",
	}
	cb := BuildCodebook(files)
	if !cb.Empty() {
		t.Fatal("expected empty codebook for < 3 files")
	}
}

func TestBuildCodebook_LineFrequency(t *testing.T) {
	line := `import "github.com/example/project/internal/store"`
	files := []string{
		line + "\nfunc A() {}",
		line + "\nfunc B() {}",
		line + "\nfunc C() {}",
	}
	cb := BuildCodebook(files)
	if cb.Empty() {
		t.Fatal("expected non-empty codebook")
	}

	legend := cb.Legend()
	if !strings.Contains(legend, "§0="+line) {
		t.Errorf("legend missing entry: %q", legend)
	}

	applied := cb.Apply(files[0])
	if strings.Contains(applied, line) {
		t.Error("Apply did not replace the repeated line")
	}
	if !strings.Contains(applied, "§0") {
		t.Error("Apply did not insert ref §0")
	}
}

func TestBuildCodebook_ShortLinesSkipped(t *testing.T) {
	files := []string{
		"}\nfunc A() {}",
		"}\nfunc B() {}",
		"}\nfunc C() {}",
	}
	cb := BuildCodebook(files)
	// "}" is < 8 chars — should not be encoded.
	if !cb.Empty() {
		t.Error("expected empty codebook: short lines should not be encoded")
	}
}

func TestBuildCodebook_TotalLineCap(t *testing.T) {
	// Build a file set that exceeds 50 000 lines.
	var big strings.Builder
	repeat := `import "github.com/example/project/internal/store"` + "\n"
	for i := 0; i < 17_000; i++ {
		big.WriteString(repeat)
	}
	s := big.String()
	files := []string{s, s, s}
	cb := BuildCodebook(files)
	if !cb.Empty() {
		t.Error("expected empty codebook when total lines > 50 000")
	}
}

func TestApply_PreservesIndent(t *testing.T) {
	line := `import "github.com/example/project/internal/store"`
	files := []string{
		"\t" + line + "\nfunc A() {}",
		"\t" + line + "\nfunc B() {}",
		"\t" + line + "\nfunc C() {}",
	}
	cb := BuildCodebook(files)
	if cb.Empty() {
		t.Fatal("expected indented line to be matched (#450: df is keyed indent-free)")
	}
	// The legend value must be indent-free; Apply re-adds the line's own indent.
	if !strings.Contains(cb.Legend(), "§0="+line) {
		t.Errorf("legend value should be indent-free: %q", cb.Legend())
	}
	applied := cb.Apply(files[0])
	for _, l := range strings.Split(applied, "\n") {
		if strings.Contains(l, "§") && !strings.HasPrefix(l, "\t") {
			t.Errorf("indent lost in line %q", l)
		}
	}
	// Round-trip must reproduce the original exactly — no doubled indent (#450).
	if got := expandCodebook(cb, applied); got != files[0] {
		t.Errorf("round-trip mismatch:\n got: %q\nwant: %q", got, files[0])
	}
}

// TestBuildCodebook_IndentInsensitiveDedup guards #450: the same logical line at
// different indents across files must collapse to ONE codebook entry (so common
// boilerplate isn't lost below the ≥3-file threshold) and round-trip exactly.
func TestBuildCodebook_IndentInsensitiveDedup(t *testing.T) {
	line := `import "github.com/example/project/internal/store"`
	files := []string{
		line + "\nfunc A() {}",          // no indent
		"\t" + line + "\nfunc B() {}",   // tab indent
		"\t\t" + line + "\nfunc C() {}", // double-tab indent
	}
	cb := BuildCodebook(files)
	if cb.Empty() {
		t.Fatal("expected one entry: same line at different indents must dedup")
	}
	if len(cb.entries) != 1 {
		t.Fatalf("expected exactly 1 codebook entry, got %d", len(cb.entries))
	}
	for i, f := range files {
		if got := expandCodebook(cb, cb.Apply(f)); got != f {
			t.Errorf("file %d round-trip mismatch:\n got: %q\nwant: %q", i, got, f)
		}
	}
}

// expandCodebook is the inverse of Apply: it replaces each "<indent>§N" with
// "<indent><entry-line>", used only to assert exact round-trips in tests.
func expandCodebook(cb Codebook, applied string) string {
	lines := strings.Split(applied, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, e := range cb.entries {
			if trimmed == e.ref {
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				lines[i] = indent + e.line
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func TestApply_LegendFormat(t *testing.T) {
	line := `import "github.com/example/project/internal/store"`
	files := []string{line + "\na", line + "\nb", line + "\nc"}
	cb := BuildCodebook(files)
	legend := cb.Legend()
	if !strings.HasPrefix(legend, "§MAP:\n") {
		t.Errorf("unexpected legend format: %q", legend)
	}
	if !strings.HasSuffix(legend, "\n") {
		t.Error("legend must end with newline")
	}
}

func TestCodebook_Empty(t *testing.T) {
	cb := Codebook{}
	if !cb.Empty() {
		t.Error("zero-value Codebook must be empty")
	}
	if cb.Legend() != "" {
		t.Error("empty codebook legend must be empty string")
	}
	text := "hello\nworld"
	if cb.Apply(text) != text {
		t.Error("empty codebook Apply must be identity")
	}
}
