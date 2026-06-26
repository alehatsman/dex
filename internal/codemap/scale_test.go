package codemap

import (
	"strings"
	"testing"
)

func TestRenderScale(t *testing.T) {
	out := RenderScale(Scale{Files: 336, Packages: 48, Symbols: 2802, CallEdges: 4209}, 0)
	if !strings.HasPrefix(out, "## scale\n") {
		t.Errorf("missing header:\n%s", out)
	}
	for _, want := range []string{"336 files", "48 packages", "2802 symbols", "4209 call edges"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// One line of content after the header.
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 1 {
		t.Errorf("scale should be a header + one line; got %d newlines:\n%s", n, out)
	}
}

func TestRenderScalePluralization(t *testing.T) {
	out := RenderScale(Scale{Files: 1, Packages: 1, Symbols: 1, CallEdges: 1}, 0)
	for _, want := range []string{"1 file ", "1 package ", "1 symbol ", "1 call edge\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("singular form missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "files") || strings.Contains(out, "call edges") {
		t.Errorf("count of 1 must not pluralize:\n%s", out)
	}
}

func TestRenderScaleOmitsEmpty(t *testing.T) {
	if RenderScale(Scale{}, 0) != "" {
		t.Error("zero scale must render no section")
	}
	if (Scale{}).Empty() != true {
		t.Error("zero Scale should report Empty")
	}
	// A partially-populated scale still renders, skipping zero fields.
	out := RenderScale(Scale{Packages: 3}, 0)
	if !strings.Contains(out, "3 packages") || strings.Contains(out, "files") {
		t.Errorf("partial scale should show only non-zero fields:\n%s", out)
	}
}

func TestRenderScaleDeterministic(t *testing.T) {
	s := Scale{Files: 10, Packages: 2, Symbols: 50, CallEdges: 80}
	if RenderScale(s, 0) != RenderScale(s, 0) {
		t.Error("RenderScale must be deterministic")
	}
}

func TestRenderScaleBudgetDropsTrailingCounts(t *testing.T) {
	full := Scale{Files: 336, Packages: 48, Symbols: 2802, CallEdges: 4209}
	// A tight budget keeps the most-orienting leading counts and drops the
	// trailing ones rather than the whole section.
	tight := RenderScale(full, 20)
	if !strings.Contains(tight, "336 files") {
		t.Errorf("tight budget dropped the leading count:\n%s", tight)
	}
	if strings.Contains(tight, "call edges") {
		t.Errorf("tight budget should have dropped trailing counts:\n%s", tight)
	}
	// At least the first count always survives (a section beats no section).
	one := RenderScale(full, 1)
	if !strings.Contains(one, "336 files") {
		t.Errorf("even a budget of 1 keeps the leading count:\n%s", one)
	}
}

// TestRenderOrientAppendsScale verifies the section is appended (not
// interleaved) and omitted byte-identically when the scale is empty.
func TestRenderOrientAppendsScale(t *testing.T) {
	cs := []Cluster{{ID: 1, Size: 2, Symbols: []Symbol{
		{QualifiedName: "Run", Kind: "function", Pkg: "main", Path: "main.go", Line: 1, PageRank: 0.9},
	}}}
	withScale := RenderOrient(cs, OrientExtras{Scale: Scale{Files: 12, Packages: 3, Symbols: 40, CallEdges: 55}}, 1000, 1000)
	without := RenderOrient(cs, OrientExtras{}, 1000, 1000)
	if !strings.Contains(withScale, "## scale") {
		t.Errorf("scale section missing:\n%s", withScale)
	}
	if strings.Contains(without, "## scale") {
		t.Errorf("empty scale must omit the section:\n%s", without)
	}
	if !strings.HasPrefix(withScale, without) {
		t.Error("scale section must be APPENDED, not interleaved")
	}
}
