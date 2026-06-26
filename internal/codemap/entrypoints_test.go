package codemap

import (
	"strings"
	"testing"
)

func TestRenderEntrypoints(t *testing.T) {
	out := RenderEntrypoints([]string{"cmd/dex/main.go", "cmd/tool/main.go"}, 0)
	if !strings.Contains(out, "## entrypoints") {
		t.Errorf("missing header:\n%s", out)
	}
	for _, want := range []string{"main — cmd/dex/main.go", "main — cmd/tool/main.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No mains → empty section (library).
	if RenderEntrypoints(nil, 0) != "" {
		t.Error("nil entrypoints should render no section")
	}
	// Deterministic.
	a := RenderEntrypoints([]string{"cmd/a/main.go", "cmd/b/main.go"}, 0)
	b := RenderEntrypoints([]string{"cmd/a/main.go", "cmd/b/main.go"}, 0)
	if a != b {
		t.Error("RenderEntrypoints must be deterministic")
	}
}

// TestRenderOrientAppendsEntrypoints verifies entrypoints land before externals
// (entry → boundary), are appended (not interleaved), and omitted when empty.
func TestRenderOrientAppendsEntrypoints(t *testing.T) {
	cs := []Cluster{{ID: 1, Size: 1, Symbols: []Symbol{
		{QualifiedName: "Run", Kind: "function", Pkg: "main", Path: "main.go", Line: 1, PageRank: 0.9},
	}}}
	extras := OrientExtras{
		Entrypoints: []string{"cmd/dex/main.go"},
		Externals:   []string{"github.com/mattn/go-sqlite3"},
	}
	full := RenderOrient(cs, extras, 1000, 1000)
	if !strings.Contains(full, "## entrypoints") || !strings.Contains(full, "## external dependencies") {
		t.Fatalf("both sections expected:\n%s", full)
	}
	// entrypoints precedes externals.
	if strings.Index(full, "## entrypoints") > strings.Index(full, "## external dependencies") {
		t.Errorf("entrypoints should come before externals:\n%s", full)
	}
	// Omitted when empty → byte-identical to the no-extras bundle.
	bare := RenderOrient(cs, OrientExtras{}, 1000, 1000)
	if strings.Contains(bare, "## entrypoints") {
		t.Errorf("empty entrypoints must omit the section:\n%s", bare)
	}
	if !strings.HasPrefix(full, bare) {
		t.Error("extras sections must be APPENDED to the bare bundle, not interleaved")
	}
}
