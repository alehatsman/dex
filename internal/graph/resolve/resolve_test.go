package resolve

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFiles materializes a map of project-relative path → content under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// monorepo lays out a small but representative workspace:
//   - a root tsconfig with a "@/*" alias (baseUrl ".") and a "@bright/common"
//     path alias, plus a base tsconfig it extends;
//   - two workspace packages named via package.json.
func monorepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"tsconfig.base.json": `{
			// shared compiler options
			"compilerOptions": {
				"baseUrl": ".",
				"paths": { "@app/*": ["apps/base-view/src/*"] }
			}
		}`,
		"tsconfig.json": `{
			"extends": "./tsconfig.base",
			"compilerOptions": {
				"baseUrl": ".",
				"paths": {
					"@/*": ["src/*"],
					"@bright/common": ["packages/bright-common/src/index.ts"],
				}
			}
		}`,
		"packages/bright-common/package.json": `{
			"name": "@bright/common",
			"module": "src/index.ts"
		}`,
		"packages/bright-ui/package.json": `{
			"name": "@bright/ui",
			"exports": { ".": { "import": "./src/index.tsx" } }
		}`,
		"apps/base-view/package.json": `{ "name": "@bright/base-view" }`,
	})
	return root
}

func TestClassify(t *testing.T) {
	w := Load(monorepo(t))

	cases := []struct {
		name       string
		specifier  string
		wantOrigin Origin
		wantPkgDir string
	}{
		{"path alias", "@app/util", OriginAlias, ""},
		{"workspace subpath", "@bright/ui/Button", OriginWorkspace, "packages/bright-ui"},
		{"bare dependency", "react", OriginExternal, ""},
		{"relative is external-ish (no candidates)", "./sibling", OriginExternal, ""},
		// @bright/common matches both an exact alias and a workspace name; the
		// alias wins, matching Candidates' precedence.
		{"alias beats workspace", "@bright/common", OriginAlias, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := w.Classify(tc.specifier)
			if c.Origin != tc.wantOrigin {
				t.Errorf("Classify(%q).Origin = %d, want %d", tc.specifier, c.Origin, tc.wantOrigin)
			}
			if c.PkgDir != tc.wantPkgDir {
				t.Errorf("Classify(%q).PkgDir = %q, want %q", tc.specifier, c.PkgDir, tc.wantPkgDir)
			}
			// Candidates must equal Classify().Candidates — same source of truth.
			if got := w.Candidates(tc.specifier); !reflect.DeepEqual(got, c.Candidates) {
				t.Errorf("Candidates(%q)=%v disagrees with Classify().Candidates=%v",
					tc.specifier, got, c.Candidates)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	w := Load(monorepo(t))

	cases := []struct {
		name      string
		specifier string
		want      []string
	}{
		{
			name:      "tsconfig glob alias with baseUrl",
			specifier: "@/util/format",
			want:      []string{"src/util/format"},
		},
		{
			name:      "inherited alias from extends chain",
			specifier: "@app/widget/Button",
			want:      []string{"apps/base-view/src/widget/Button"},
		},
		{
			name:      "exact tsconfig alias (no star) beats workspace name",
			specifier: "@bright/common",
			// exact alias fires first; workspace entries follow.
			want: []string{
				"packages/bright-common/src/index",
				"packages/bright-common/index",
				"packages/bright-common/src/main",
				"packages/bright-common/lib/index",
			},
		},
		{
			name:      "workspace subpath import",
			specifier: "@bright/ui/Button",
			want:      []string{"packages/bright-ui/Button", "packages/bright-ui/src/Button"},
		},
		{
			name:      "workspace bare import via exports",
			specifier: "@bright/ui",
			want: []string{
				"packages/bright-ui/src/index",
				"packages/bright-ui/index",
				"packages/bright-ui/src/main",
				"packages/bright-ui/lib/index",
			},
		},
		{
			name:      "bare npm dep is external (no candidates)",
			specifier: "@mui/material",
			want:      nil,
		},
		{
			name:      "unscoped bare dep is external",
			specifier: "react",
			want:      nil,
		},
		{
			name:      "relative specifier is not our job",
			specifier: "./sibling",
			want:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := w.Candidates(tc.specifier)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Candidates(%q)\n got: %#v\nwant: %#v", tc.specifier, got, tc.want)
			}
		})
	}
}

// TestSubpathExports covers #130: exact exports subpath keys resolve through a
// build→src retarget and, for a compiled re-export barrel, one hop to the real
// (possibly differently-named) source.
func TestSubpathExports(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"packages/bright-common/package.json": `{
			"name": "@bright/common",
			"exports": {
				".": { "import": "./build/index.js" },
				"./Uuid":   { "import": "./build/Uuid.js", "types": "./build/Uuid.d.ts" },
				"./Direct": { "import": "./build/Direct.js" }
			}
		}`,
		// A compiled re-export barrel whose source is differently named.
		"packages/bright-common/build/Uuid.js":    "export * from './UuidCodec.js';\nexport { default } from './UuidCodec.js';\n",
		"packages/bright-common/src/UuidCodec.ts": "export class Uuid {}\n",
		// A same-named source: build→src path rewrite alone suffices.
		"packages/bright-common/src/Direct.ts": "export const x = 1\n",
	})
	w := Load(root)

	assertHas := func(spec, want string) {
		t.Helper()
		got := w.Candidates(spec)
		if slicesContains(got, want) {
			return
		}
		t.Errorf("Candidates(%q) = %v, missing %q", spec, got, want)
	}
	// Barrel follow: ./Uuid → build/Uuid.js → export * from './UuidCodec' → src/UuidCodec.
	assertHas("@bright/common/Uuid", "packages/bright-common/src/UuidCodec")
	// build→src retarget for a same-named source.
	assertHas("@bright/common/Direct", "packages/bright-common/src/Direct")
	// An import written with an explicit .js ext still matches the "Uuid" key.
	assertHas("@bright/common/Uuid.js", "packages/bright-common/src/UuidCodec")
}

// TestSubpathExportsFallbackWhenUnbuilt: with an exports subpath but no artifact
// on disk (a fresh, unbuilt checkout), resolution still offers the generic
// workspace probes — no regression, no barrel crash.
func TestSubpathExportsFallbackWhenUnbuilt(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"packages/p/package.json": `{
			"name": "@x/p",
			"exports": { "./Foo": { "import": "./build/Foo.js" } }
		}`,
	})
	got := Load(root).Candidates("@x/p/Foo")
	for _, want := range []string{"packages/p/Foo", "packages/p/src/Foo"} {
		if !slicesContains(got, want) {
			t.Errorf("Candidates(@x/p/Foo)=%v missing generic probe %q", got, want)
		}
	}
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestExactAliasPrecedence pins that "@bright/common" resolves the exact alias
// target FIRST (index), guarding the most-specific-first ordering.
func TestExactAliasPrecedence(t *testing.T) {
	got := Load(monorepo(t)).Candidates("@bright/common")
	if len(got) == 0 || got[0] != "packages/bright-common/src/index" {
		t.Fatalf("expected exact alias target first, got %#v", got)
	}
}

// TestEmptyWorkspace: a repo with no package.json/tsconfig resolves nothing, so
// every non-relative specifier stays external (no regression for plain repos).
func TestEmptyWorkspace(t *testing.T) {
	w := Load(t.TempDir())
	for _, spec := range []string{"@bright/common", "@/util", "react"} {
		if got := w.Candidates(spec); got != nil {
			t.Errorf("empty workspace Candidates(%q) = %#v, want nil", spec, got)
		}
	}
	// nil receiver is safe too.
	if got := (*Workspace)(nil).Candidates("@x/y"); got != nil {
		t.Errorf("nil Workspace Candidates = %#v, want nil", got)
	}
}

// TestProjects: Projects() lists every workspace package (name + dir); the empty
// / nil workspace yields nil.
func TestProjects(t *testing.T) {
	got := map[string]string{}
	for _, p := range Load(monorepo(t)).Projects() {
		got[p.Name] = p.Dir
	}
	want := map[string]string{
		"@bright/common":    "packages/bright-common",
		"@bright/ui":        "packages/bright-ui",
		"@bright/base-view": "apps/base-view",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Projects() = %#v, want %#v", got, want)
	}
	if p := Load(t.TempDir()).Projects(); p != nil {
		t.Errorf("empty workspace Projects() = %#v, want nil", p)
	}
	if p := (*Workspace)(nil).Projects(); p != nil {
		t.Errorf("nil Workspace Projects() = %#v, want nil", p)
	}
}

// TestProjectOf: a module path maps to its owning workspace project by
// longest path-boundary prefix; unowned paths and boundary near-misses map to "".
func TestProjectOf(t *testing.T) {
	w := Load(monorepo(t))
	cases := map[string]string{
		"packages/bright-common/src/index":   "@bright/common",
		"packages/bright-common":             "@bright/common", // exact dir
		"packages/bright-ui/src/Button":      "@bright/ui",
		"apps/base-view/src/main":            "@bright/base-view",
		"packages/bright-common-extra/src/x": "", // boundary: not bright-common
		"src/util":                           "", // owned by no package
		"":                                   "",
	}
	for p, want := range cases {
		if got := w.ProjectOf(p); got != want {
			t.Errorf("ProjectOf(%q) = %q, want %q", p, got, want)
		}
	}
	if got := (*Workspace)(nil).ProjectOf("packages/bright-ui/src/x"); got != "" {
		t.Errorf("nil Workspace ProjectOf = %q, want \"\"", got)
	}
}

// TestSkipsNodeModules: a package.json under node_modules must not register as a
// workspace package (no node_modules resolution).
func TestSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"node_modules/@bright/common/package.json": `{ "name": "@bright/common" }`,
	})
	if got := Load(root).Candidates("@bright/common"); got != nil {
		t.Errorf("node_modules package leaked into workspace: %#v", got)
	}
}

// TestJSONCTolerance: comments and trailing commas in tsconfig must not break
// alias loading.
func TestJSONCTolerance(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"tsconfig.json": `{
			/* block comment */
			"compilerOptions": {
				"baseUrl": "./",
				"paths": {
					"@/*": ["src/*"], // trailing comma below
				},
			},
		}`,
	})
	got := Load(root).Candidates("@/thing")
	want := []string{"src/thing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSONC alias: got %#v want %#v", got, want)
	}
}
