package mcp

import (
	"context"
	"strings"
	"testing"
)

// manifestFixture indexes a project whose go.mod carries a real require block,
// and returns the resolved project root and cache dir. The dependency-manifest
// shortcut (#125) recognises go.mod by name, so the file content only needs to
// be a parseable module file with deps to exercise both code paths.
func manifestFixture(t *testing.T) (projRoot, cacheDir string) {
	t.Helper()
	embedSrv := fakeEmbed(t, 16)
	t.Cleanup(embedSrv.Close)

	const goMod = "module example.com/m\n\ngo 1.21\n\nrequire (\n\tgithub.com/foo/bar v1.2.3\n\tgithub.com/baz/qux v0.4.0\n)\n"
	projDir := t.TempDir()
	writeFile(t, projDir+"/go.mod", goMod)
	// A source file so the index has something to embed besides the manifest.
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	cacheDir = t.TempDir()
	projRoot = indexProject(t, projDir, cacheDir, embedSrv.URL)
	return projRoot, cacheDir
}

// TestManifestExplicitModeWins locks issue #511: an explicitly-requested read
// mode must be honored on a dependency manifest. Before the fix the deps
// shortcut (#125) fired for every structural mode, so `lines:N-M` (and
// signatures/skeleton/map/aggressive) silently returned the compacted
// "Go (module …)" summary instead of the requested view.
func TestManifestExplicitModeWins(t *testing.T) {
	projRoot, cacheDir := manifestFixture(t)
	ctx := context.Background()
	s := chatDownServer(t, cacheDir, nil)

	// The deps-summary header CompressGoMod emits; its presence means the
	// shortcut fired.
	const depsHeader = "Go ("

	t.Run("explicit lines returns the raw slice", func(t *testing.T) {
		_, out, err := s.summarize(ctx, nil, SummarizeInput{
			Path: "go.mod", Mode: "lines:1-3", ProjectRoot: projRoot,
		})
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}
		if out.Status != "ok" {
			t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
		}
		if strings.Contains(out.Content, depsHeader) {
			t.Errorf("explicit lines:1-3 was preempted by the deps shortcut; got:\n%s", out.Content)
		}
		if !strings.Contains(out.Content, "module example.com/m") {
			t.Errorf("lines:1-3 missing the raw module line; got:\n%s", out.Content)
		}
		// The require block starts at line 5 — it must NOT appear in a 1-3 slice.
		if strings.Contains(out.Content, "github.com/foo/bar") {
			t.Errorf("lines:1-3 leaked content past line 3; got:\n%s", out.Content)
		}
	})

	t.Run("explicit signatures is not the deps summary", func(t *testing.T) {
		_, out, err := s.summarize(ctx, nil, SummarizeInput{
			Path: "go.mod", Mode: "signatures", ProjectRoot: projRoot,
		})
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}
		if out.Status != "ok" {
			t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
		}
		if strings.Contains(out.Content, depsHeader) {
			t.Errorf("explicit signatures was preempted by the deps shortcut; got:\n%s", out.Content)
		}
	})
}

// TestManifestAutoModeStillCompacts is the other half of #511: when the
// structural mode was auto-selected (here via a task hint, which leaves
// in.Mode empty), the deps shortcut must still fire so a manifest read stays
// compact. The fix only suppresses the shortcut for explicit requests.
func TestManifestAutoModeStillCompacts(t *testing.T) {
	projRoot, cacheDir := manifestFixture(t)
	ctx := context.Background()
	s := chatDownServer(t, cacheDir, nil)

	// No explicit Mode; the "implement" task hint resolves to a structural
	// mode (aggressive), so the deps shortcut should engage.
	_, out, err := s.summarize(ctx, nil, SummarizeInput{
		Path: "go.mod", Task: "implement the new feature", ProjectRoot: projRoot,
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
	}
	if !strings.Contains(out.Content, "Go (") {
		t.Errorf("auto-selected structural mode did not compact the manifest; got:\n%s", out.Content)
	}
}
