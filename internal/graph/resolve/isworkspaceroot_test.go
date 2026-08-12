package resolve

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsWorkspaceRoot: a root is a workspace only with a top-level workspace
// manifest — not because Load can find a buried package.json (the #151 fixture
// trap that would otherwise mislabel a Go repo).
func TestIsWorkspaceRoot(t *testing.T) {
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name  string
		setup func(dir string)
		want  bool
	}{
		{"rush.json", func(d string) { write(d, "rush.json", "{}") }, true},
		{"pnpm-workspace.yaml", func(d string) { write(d, "pnpm-workspace.yaml", "packages:\n  - 'p/*'\n") }, true},
		{"lerna.json", func(d string) { write(d, "lerna.json", "{}") }, true},
		{"package.json with workspaces array", func(d string) { write(d, "package.json", `{"workspaces":["packages/*"]}`) }, true},
		{"package.json with workspaces object", func(d string) { write(d, "package.json", `{"workspaces":{"packages":["p/*"]}}`) }, true},
		{"package.json without workspaces", func(d string) { write(d, "package.json", `{"name":"x"}`) }, false},
		{"go repo (go.mod only)", func(d string) { write(d, "go.mod", "module x\n") }, false},
		{"nested fixture package.json only", func(d string) {
			sub := filepath.Join(d, "testdata", "fixture")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			write(d, "go.mod", "module x\n")
			write(sub, "package.json", `{"name":"@bright/common","workspaces":["x"]}`)
		}, false},
		{"empty root", func(string) {}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			if got := IsWorkspaceRoot(dir); got != tc.want {
				t.Errorf("IsWorkspaceRoot = %v, want %v", got, tc.want)
			}
		})
	}

	if IsWorkspaceRoot("") {
		t.Error(`IsWorkspaceRoot("") = true, want false`)
	}
}
