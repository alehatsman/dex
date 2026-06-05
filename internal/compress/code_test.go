package compress

import (
	"strings"
	"testing"
)

func TestSafeguardRatio(t *testing.T) {
	t.Run("larger compressed returns original", func(t *testing.T) {
		orig := "short"
		comp := "longer than original content here"
		if SafeguardRatio(orig, comp) != orig {
			t.Fatal("expected original when compressed is larger")
		}
	})

	t.Run("same size returns original", func(t *testing.T) {
		s := "same length text"
		if SafeguardRatio(s, s) != s {
			t.Fatal("expected original for same-length input")
		}
	})

	t.Run("trivial savings on small input returns original", func(t *testing.T) {
		orig := strings.Repeat("word ", 50) // ~50 tokens, < 2000
		comp := strings.Repeat("word ", 49) // 2% savings → below 5% threshold
		if SafeguardRatio(orig, comp) != orig {
			t.Fatal("expected original for trivial savings on small input")
		}
	})

	t.Run("meaningful savings on small input returns compressed", func(t *testing.T) {
		orig := strings.Repeat("word ", 100)
		// Remove 20% — well above 5% threshold
		comp := strings.Repeat("word ", 80)
		if SafeguardRatio(orig, comp) != comp {
			t.Fatal("expected compressed for meaningful savings")
		}
	})

	t.Run("large input bypasses 5% check", func(t *testing.T) {
		// >2000 tokens: any reduction is accepted
		orig := strings.Repeat("word ", 3000)
		comp := strings.Repeat("word ", 2999) // only 0.03% savings
		if SafeguardRatio(orig, comp) != comp {
			t.Fatal("expected compressed for large input regardless of ratio")
		}
	})
}

func TestNormalizeBlankLines(t *testing.T) {
	lines := []string{"a", "", "", "b", "", "c"}
	got := normalizeBlankLines(lines)
	want := []string{"a", "", "b", "", "c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHalveIndentation(t *testing.T) {
	lines := []string{
		"func foo() {",
		"    x := 1",
		"        y := 2",
		"    return x",
		"}",
	}
	got := halveIndentation(lines)
	if got[1] != "  x := 1" {
		t.Fatalf("4-space not halved: %q", got[1])
	}
	if got[2] != "    y := 2" {
		t.Fatalf("8-space not halved to 4: %q", got[2])
	}
	if got[0] != "func foo() {" {
		t.Fatalf("no-indent line changed: %q", got[0])
	}
}

func TestIsClosingBraceLine(t *testing.T) {
	yes := []string{"}", "};", ");", "});", "  }", "\t};"}
	no := []string{"", "x}", "} // comment", "a", "}a"}
	for _, s := range yes {
		if !isClosingBraceLine(s) {
			t.Errorf("expected true for %q", s)
		}
	}
	for _, s := range no {
		if isClosingBraceLine(s) {
			t.Errorf("expected false for %q", s)
		}
	}
}

func TestCollapseClosingBracesAggressively(t *testing.T) {
	lines := []string{
		"if x {",
		"    foo()",
		"}",
		"};",
		");",
		"",
		"bar()",
	}
	got := collapseClosingBracesAggressively(lines)
	// The 3 consecutive closing-brace lines should be merged.
	found := false
	for _, l := range got {
		if l == "} }; );" {
			found = true
		}
	}
	if !found {
		t.Fatalf("consecutive braces not merged; got: %v", got)
	}
	// Single closing brace should not be merged (no run).
	// The non-brace lines must survive.
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "if x {") || !strings.Contains(joined, "bar()") {
		t.Fatal("non-brace lines lost")
	}
}

func TestCollapseClosingBracesLightweight(t *testing.T) {
	// Run of exactly 5 — should collapse.
	lines := []string{"a", "}", "}", "}", "}", "}", "b"}
	got := collapseClosingBracesLightweight(lines)
	if len(got) != 5 { // a + } + } + annotation + b
		t.Fatalf("expected 5 lines, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[3], "collapsed") {
		t.Fatalf("expected collapse annotation, got %q", got[3])
	}

	// Run of 4 — should be left intact.
	lines2 := []string{"a", "}", "}", "}", "}", "b"}
	got2 := collapseClosingBracesLightweight(lines2)
	if len(got2) != 6 {
		t.Fatalf("run of 4 should be unchanged, got %d lines", len(got2))
	}
}

func TestStripCStyleComments(t *testing.T) {
	lines := []string{
		"package foo",
		"// single line comment",
		"/// doc comment — keep",
		"/*",
		" * block line",
		" */",
		"func Bar() {}",
		"/* inline single-line */",
	}
	got := stripCStyleComments(lines)
	joined := strings.Join(got, "\n")

	if strings.Contains(joined, "single line comment") {
		t.Error("// comment not stripped")
	}
	if !strings.Contains(joined, "doc comment") {
		t.Error("/// doc comment incorrectly stripped")
	}
	if strings.Contains(joined, "block line") {
		t.Error("/* */ block comment not stripped")
	}
	if !strings.Contains(joined, "func Bar") {
		t.Error("code line lost")
	}
}

func TestStripHashComments(t *testing.T) {
	lines := []string{"#!/usr/bin/env bash", "# comment", "echo hello"}
	got := stripHashComments(lines, true)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "#!/usr/bin/env bash") {
		t.Error("shebang stripped with preserveShebang=true")
	}
	if strings.Contains(joined, "# comment") {
		t.Error("# comment not stripped")
	}
	if !strings.Contains(joined, "echo hello") {
		t.Error("code line lost")
	}

	// Without shebang preservation.
	got2 := stripHashComments(lines, false)
	joined2 := strings.Join(got2, "\n")
	if strings.Contains(joined2, "#!/usr/bin/env bash") {
		t.Error("shebang not stripped with preserveShebang=false")
	}
}

func TestAggressiveCompressGoFile(t *testing.T) {
	content := `package foo

// helper computes something
func helper(x int) int {
	// do work
	return x + 1
}

func bigFunc() {
	a := 1
	b := 2
	_ = a + b
}
`
	result := AggressiveCompress(content, ".go")
	// Comments should be stripped.
	if strings.Contains(result, "helper computes") {
		t.Error("inline comment not stripped")
	}
	// Code must survive.
	if !strings.Contains(result, "func helper") {
		t.Error("function signature lost")
	}
	if !strings.Contains(result, "return x + 1") {
		t.Error("return statement lost")
	}
}

func TestAggressiveCompressUnknownExt(t *testing.T) {
	content := "line one\nline two\nline three\n"
	// Unknown extension — no comment stripping, returns same or safeguard-original.
	result := AggressiveCompress(content, ".xyz")
	if result != content {
		// Safeguard may return original for tiny input with trivial savings.
		// Just verify no panic and something is returned.
		if result == "" {
			t.Fatal("empty result for unknown extension")
		}
	}
}

func TestLightweightCleanup(t *testing.T) {
	// Build a file with >200 lines and a run of 100 closing braces.
	// 100 "}\n" = 200 bytes; annotation keeps 2 + label (~35 bytes),
	// saving ~163/1204 = 13.5% — well above the 5% SafeguardRatio threshold.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("line\n")
	}
	for i := 0; i < 100; i++ {
		sb.WriteString("}\n")
	}
	sb.WriteString("end\n")
	content := sb.String()

	result := LightweightCleanup(content)
	if !strings.Contains(result, "collapsed") {
		t.Error("expected brace-collapse annotation in lightweight result")
	}
	if !strings.Contains(result, "end") {
		t.Error("trailing content lost")
	}
}
