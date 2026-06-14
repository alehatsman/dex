package mcp

import (
	"context"
	"path/filepath"
	"testing"
)

// TestContextRouterExpandNoopWhenNoClient covers the documented contract for
// the per-request expand override (#252): "Requires DEX_EXPAND_MODEL to be
// configured; otherwise a no-op." With no expansion client wired, asking with
// expand=on|full must return cleanly (status ok, Expanded=false) rather than
// panic — the regression behind #502.
func TestContextRouterExpandNoopWhenNoClient(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir) // ExpandClient is nil

	for _, mode := range []string{"on", "full"} {
		t.Run(mode, func(t *testing.T) {
			_, out, err := s.ContextRouter(context.Background(), ContextInput{
				Question:    "where do we greet users",
				ProjectRoot: root,
				Expand:      mode,
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "ok" {
				t.Fatalf("status=%s hint=%s", out.Status, out.Hint)
			}
			if out.Expanded {
				t.Errorf("Expanded=true with no expand client; want false (no-op)")
			}
		})
	}
}
