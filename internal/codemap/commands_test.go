package codemap

import (
	"strings"
	"testing"
)

func TestRenderCommandsEmptyOmitsSection(t *testing.T) {
	if got := RenderCommands(nil, DefaultCommandsBudget); got != "" {
		t.Errorf("no commands must render nothing, got %q", got)
	}
	if got := RenderCommands([]Command{}, DefaultCommandsBudget); got != "" {
		t.Errorf("empty commands must render nothing, got %q", got)
	}
}

func TestRenderCommandsListsRoleAndCommand(t *testing.T) {
	out := RenderCommands([]Command{
		{Label: "build", Cmd: "mooncake task install"},
		{Label: "test", Cmd: "mooncake task test"},
	}, DefaultCommandsBudget)
	if !strings.HasPrefix(out, "## commands\n") {
		t.Errorf("missing header:\n%s", out)
	}
	for _, want := range []string{"- build: `mooncake task install`", "- test: `mooncake task test`"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// The commands section must land in RenderOrient's "entry" band — after
// entrypoints, before layers — and vanish byte-for-byte when absent.
func TestRenderOrientPlacesCommandsBetweenEntrypointsAndLayers(t *testing.T) {
	cs := []Cluster{{ID: 1, Size: 2, Symbols: []Symbol{
		{QualifiedName: "Run", Kind: "function", Pkg: "main", Path: "main.go", Line: 1, PageRank: 0.9},
	}}}
	extras := OrientExtras{
		Entrypoints: []string{"cmd/dex/main.go"},
		Commands:    []Command{{Label: "test", Cmd: "go test ./..."}},
		ImportEdges: []ImportEdge{{From: "a", To: "b"}},
	}
	full := RenderOrient(cs, extras, 1000, 1000)
	ci := strings.Index(full, "## commands")
	if ci < 0 {
		t.Fatalf("commands section missing:\n%s", full)
	}
	if ei := strings.Index(full, "## entrypoints"); ei < 0 || ei > ci {
		t.Errorf("commands must follow entrypoints (ep=%d cmd=%d):\n%s", ei, ci, full)
	}
	if li := strings.Index(full, "## layers"); li >= 0 && li < ci {
		t.Errorf("commands must precede layers (cmd=%d layers=%d):\n%s", ci, li, full)
	}

	bare := RenderOrient(cs, OrientExtras{Entrypoints: extras.Entrypoints, ImportEdges: extras.ImportEdges}, 1000, 1000)
	if strings.Contains(bare, "## commands") {
		t.Errorf("empty commands must omit the section:\n%s", bare)
	}
}
