package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// makeLargeFile builds an n-line file. If changeLine >= 0, that line is replaced
// with newLine in the second returned value.
func makeLargeFile(n, changeLine int, newLine string) (old, new []byte) {
	var ob, nb strings.Builder
	for i := 1; i <= n; i++ {
		line := fmt.Sprintf("package body line %04d: some realistic content here\n", i)
		ob.WriteString(line)
		if i-1 == changeLine {
			nb.WriteString(newLine + "\n")
		} else {
			nb.WriteString(line)
		}
	}
	return []byte(ob.String()), []byte(nb.String())
}

func TestComputeLineDelta_NoChange(t *testing.T) {
	data := []byte("line1\nline2\nline3\n")
	diff, ok := computeLineDelta(data, data)
	if ok {
		t.Errorf("expected ok=false for identical content, got diff=%q", diff)
	}
}

func TestComputeLineDelta_SingleLineChange(t *testing.T) {
	old, new := makeLargeFile(80, 40, "// THIS LINE WAS CHANGED by the edit")
	diff, ok := computeLineDelta(old, new)
	if !ok {
		t.Fatalf("expected ok=true for single-line change in 80-line file; diff len=%d full len=%d", len(diff), len(new))
	}
	if !strings.Contains(diff, "-package body line 0041") {
		t.Errorf("diff missing removal of original line: %q", diff)
	}
	if !strings.Contains(diff, "+// THIS LINE WAS CHANGED") {
		t.Errorf("diff missing addition marker: %q", diff)
	}
	if len(diff) >= len(new) {
		t.Errorf("diff len %d >= full len %d — not compact", len(diff), len(new))
	}
}

func TestComputeLineDelta_LargeChange_NotWorth(t *testing.T) {
	// Replace every line — diff will be as large as both files combined.
	var ob, nb strings.Builder
	for i := 0; i < 40; i++ {
		ob.WriteString("original content line here with some words\n")
		nb.WriteString("completely replaced with different text\n")
	}
	_, ok := computeLineDelta([]byte(ob.String()), []byte(nb.String()))
	if ok {
		t.Errorf("expected ok=false when diff exceeds threshold")
	}
}

func TestComputeLineDelta_LineAdded(t *testing.T) {
	old, _ := makeLargeFile(80, -1, "")
	// Insert a line in the middle.
	lines := strings.Split(string(old), "\n")
	lines = append(lines[:40], append([]string{"// INSERTED LINE"}, lines[40:]...)...)
	new := []byte(strings.Join(lines, "\n"))

	diff, ok := computeLineDelta(old, new)
	if !ok {
		t.Fatalf("expected ok=true for single insertion; diff len=%d full len=%d", len(diff), len(new))
	}
	if !strings.Contains(diff, "+// INSERTED LINE") {
		t.Errorf("diff missing +INSERTED LINE: %q", diff)
	}
}

func TestComputeLineDelta_LineDeleted(t *testing.T) {
	old, _ := makeLargeFile(80, -1, "")
	// Delete line 40.
	lines := strings.Split(strings.TrimRight(string(old), "\n"), "\n")
	new := []byte(strings.Join(append(lines[:40], lines[41:]...), "\n") + "\n")

	diff, ok := computeLineDelta(old, new)
	if !ok {
		t.Fatalf("expected ok=true for single deletion; diff len=%d full len=%d", len(diff), len(new))
	}
	if !strings.Contains(diff, "-package body line 0041") {
		t.Errorf("diff missing -deleted line: %q", diff)
	}
}

func TestUnifiedDiff_HunkHeader(t *testing.T) {
	old := []string{"a", "b", "c", "d", "e"}
	new := []string{"a", "b", "X", "d", "e"}
	diff := unifiedDiff(old, new, 1)
	if !strings.HasPrefix(diff, "@@") {
		t.Errorf("expected hunk header, got: %q", diff)
	}
	if !strings.Contains(diff, "-c") {
		t.Errorf("expected -c in diff: %q", diff)
	}
	if !strings.Contains(diff, "+X") {
		t.Errorf("expected +X in diff: %q", diff)
	}
}

func TestComputeLineDelta_TooLarge_Skipped(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < deltaMaxLines+10; i++ {
		sb.WriteString("line\n")
	}
	big := []byte(sb.String())
	modified := []byte(sb.String()[:sb.Len()-10] + "changed\n")
	_, ok := computeLineDelta(big, modified)
	if ok {
		t.Errorf("expected ok=false for files exceeding deltaMaxLines")
	}
}
