package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageGraphProjectLevelGate locks the #154 fix: the project rollup must
// only run on a real JS/TS workspace root. resolve.Load walks the whole tree for
// package.json, so on a non-workspace repo (a Go module with buried JS/TS test
// fixtures) it would fabricate bogus workspace projects. The gate is decided
// before any index work — the workspace-root fact is index-independent — so this
// test needs no index at all.
func TestPackageGraphProjectLevelGate(t *testing.T) {
	// Non-workspace root: go.mod at the root, a buried package.json fixture. The
	// gate must short-circuit to no-graph with a workspace-root hint, never
	// reaching resolve.Load / BuildProjectGraph.
	nonWS := t.TempDir()
	writeFile(t, filepath.Join(nonWS, "go.mod"), "module example.com/x\n\ngo 1.24\n")
	writeFile(t, filepath.Join(nonWS, "internal", "testdata", "pkg", "package.json"),
		`{"name":"@acme/fixture","version":"0.0.0"}`)

	s := stubServer(t)
	_, out, err := s.packageGraph(context.Background(), nil, PackageGraphInput{ProjectRoot: nonWS, Level: "project"})
	if err != nil {
		t.Fatalf("packageGraph: %v", err)
	}
	if out.Status != "no-graph" {
		t.Fatalf("non-workspace project level: status = %q, want no-graph", out.Status)
	}
	if len(out.Nodes) != 0 {
		t.Fatalf("non-workspace project level leaked %d fixture nodes: %+v", len(out.Nodes), out.Nodes)
	}
	if !strings.Contains(out.Hint, "workspace root") {
		t.Fatalf("expected a workspace-root hint, got %q", out.Hint)
	}

	// Real workspace root: package.json with a "workspaces" field at the root.
	// The gate must let it through — with no index present it then reports
	// no-index, proving the gate did NOT block a legitimate workspace.
	ws := t.TempDir()
	writeFile(t, filepath.Join(ws, "package.json"),
		`{"name":"root","private":true,"workspaces":["packages/*"]}`)

	_, wout, err := s.packageGraph(context.Background(), nil, PackageGraphInput{ProjectRoot: ws, Level: "project"})
	if err != nil {
		t.Fatalf("packageGraph (workspace): %v", err)
	}
	if wout.Status == "no-graph" && strings.Contains(wout.Hint, "workspace root") {
		t.Fatalf("workspace root was wrongly gated out: %q", wout.Hint)
	}
	if wout.Status != "no-index" {
		t.Fatalf("workspace root with no index: status = %q, want no-index (gate passed)", wout.Status)
	}
}
