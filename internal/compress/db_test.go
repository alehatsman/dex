package compress

import (
	"fmt"
	"strings"
	"testing"
)

func mysqlTable(rows int) []string {
	l := []string{"+----+------+", "| id | name |", "+----+------+"}
	for i := 0; i < rows; i++ {
		l = append(l, fmt.Sprintf("| %2d | r%-3d |", i, i))
	}
	return append(l, "+----+------+")
}

func psqlTable(rows int) []string {
	l := []string{" id | name", "----+------"}
	for i := 0; i < rows; i++ {
		l = append(l, fmt.Sprintf("  %d | r%d", i, i))
	}
	return append(l, fmt.Sprintf("(%d rows)", rows))
}

func assertUnmutated(t *testing.T, fn string, got, orig []string) {
	t.Helper()
	if len(got) != len(orig) {
		t.Fatalf("%s changed caller slice length: %d -> %d", fn, len(orig), len(got))
	}
	for i := range orig {
		if got[i] != orig[i] {
			t.Fatalf("%s mutated caller input at %d: got %q, want %q", fn, i, got[i], orig[i])
		}
	}
}

func TestCompressMySQLTable_DoesNotMutateInput(t *testing.T) {
	in := mysqlTable(25)
	orig := append([]string(nil), in...)

	out := CompressMySQLTable(in)

	// The >20-row branch must have truncated (otherwise the append site
	// isn't exercised and the test proves nothing).
	if len(out) >= len(in) {
		t.Fatalf("expected truncation, got len(out)=%d >= len(in)=%d", len(out), len(in))
	}
	if !strings.Contains(out[len(out)-1], "rows total") {
		t.Errorf("last output line should be the summary, got %q", out[len(out)-1])
	}
	assertUnmutated(t, "CompressMySQLTable", in, orig)
}

func TestCompressPsqlTable_DoesNotMutateInput(t *testing.T) {
	in := psqlTable(25)
	orig := append([]string(nil), in...)

	out := CompressPsqlTable(in)

	if len(out) >= len(in) {
		t.Fatalf("expected truncation, got len(out)=%d >= len(in)=%d", len(out), len(in))
	}
	assertUnmutated(t, "CompressPsqlTable", in, orig)
}
