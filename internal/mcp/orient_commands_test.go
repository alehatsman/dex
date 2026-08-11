package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alehatsman/dex/internal/codemap"
)

// asMap flattens extracted commands to role→command for order-independent
// assertions; a separate case pins ordering.
func asMap(cmds []codemap.Command) map[string]string {
	m := map[string]string{}
	for _, c := range cmds {
		m[c.Label] = c.Cmd
	}
	return m
}

func writeRepoFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractProjectCommands(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  map[string]string
	}{
		{
			name: "mooncake tasks.yml",
			files: map[string]string{
				"tasks.yml": "vars:\n  GO_TAGS: sqlite_fts5\ntasks:\n  install:\n    desc: build\n    steps:\n      - shell: go build\n  test: goq/test\n  ci-fast:\n    desc: gate\n  lint: goq/lint\nmodules:\n  goq:\n    source: x\n",
			},
			want: map[string]string{
				"build": "mooncake task install",
				"test":  "mooncake task test",
				"ci":    "mooncake task ci-fast",
				"lint":  "mooncake task lint",
			},
		},
		{
			name: "Makefile",
			files: map[string]string{
				"Makefile": ".PHONY: build test\nbuild:\n\tgo build ./...\ntest:\n\tgo test ./...\nrun:\n\t./bin/app\n",
			},
			want: map[string]string{
				"build": "make build",
				"test":  "make test",
				"run":   "make run",
			},
		},
		{
			name: "package.json scripts",
			files: map[string]string{
				"package.json": `{"scripts":{"build":"tsc","test":"jest","dev":"vite","format":"prettier"}}`,
			},
			want: map[string]string{
				"build": "npm run build",
				"test":  "npm run test",
				"run":   "npm run dev",
			},
		},
		{
			name:  "bare go module fallback",
			files: map[string]string{"go.mod": "module x\n\ngo 1.26\n"},
			want:  map[string]string{"build": "go build ./...", "test": "go test ./..."},
		},
		{
			name:  "empty repo yields nothing",
			files: map[string]string{"README.md": "# x\n"},
			want:  map[string]string{},
		},
		{
			name: "task runner suppresses language fallback",
			files: map[string]string{
				"go.mod":   "module x\n",
				"Makefile": "test:\n\tgo test ./...\n",
			},
			want: map[string]string{"test": "make test"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tc.files {
				writeRepoFile(t, root, name, body)
			}
			got := asMap(ExtractProjectCommands(root))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for role, cmd := range tc.want {
				if got[role] != cmd {
					t.Errorf("role %s = %q, want %q", role, got[role], cmd)
				}
			}
		})
	}
}

// tasks.yml wins over Makefile wins over package.json for a shared role.
func TestExtractProjectCommandsSourcePriority(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "tasks.yml", "tasks:\n  test: goq/test\n")
	writeRepoFile(t, root, "Makefile", "test:\n\tgo test\n")
	writeRepoFile(t, root, "package.json", `{"scripts":{"test":"jest"}}`)
	if got := asMap(ExtractProjectCommands(root))["test"]; got != "mooncake task test" {
		t.Errorf("test = %q, want mooncake task test (tasks.yml priority)", got)
	}
}

// The rendered order is fixed (build, test, lint, run, ci) regardless of source
// order, so the orientation section is cache-stable.
func TestExtractProjectCommandsOrder(t *testing.T) {
	root := t.TempDir()
	writeRepoFile(t, root, "Makefile", "run:\n\t./x\nlint:\n\tvet\ntest:\n\tt\nbuild:\n\tb\n")
	cmds := ExtractProjectCommands(root)
	var order []string
	for _, c := range cmds {
		order = append(order, c.Label)
	}
	want := []string{"build", "test", "lint", "run"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
