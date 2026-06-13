package mcp

import (
	"strings"
	"testing"
)

// ─── depsFileArg ──────────────────────────────────────────────────────────────

func TestDepsFileArg(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"cat package.json", "package.json"},
		{"cat path/to/go.mod", "go.mod"},
		{"cat", ""},
		{"", ""},
		{"head -n 20 Cargo.toml", "Cargo.toml"},
	}
	for _, c := range cases {
		if got := depsFileArg(c.cmd); got != c.want {
			t.Errorf("depsFileArg(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

// ─── isDepsFilename ───────────────────────────────────────────────────────────

func TestIsDepsFilename(t *testing.T) {
	yes := []string{
		"package.json", "go.mod", "go.sum", "Cargo.toml", "Cargo.lock",
		"requirements.txt", "requirements-dev.txt", "pyproject.toml",
		"Pipfile", "Gemfile", "Gemfile.lock", "pom.xml", "build.gradle",
		"build.gradle.kts", "composer.json", "package-lock.json", "yarn.lock",
		"pnpm-lock.yaml", "bun.lockb",
	}
	for _, name := range yes {
		if !isDepsFilename(name) {
			t.Errorf("isDepsFilename(%q) = false, want true", name)
		}
	}
	no := []string{"main.go", "README.md", "Dockerfile", "setup.py", ""}
	for _, name := range no {
		if isDepsFilename(name) {
			t.Errorf("isDepsFilename(%q) = true, want false", name)
		}
	}
}

// ─── compressGoMod ────────────────────────────────────────────────────────────

func TestCompressGoMod(t *testing.T) {
	gomod := `module github.com/example/myapp

go 1.21

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.9.0 // indirect
)
`
	out, ok := compressGoMod([]byte(gomod))
	if !ok {
		t.Fatal("compressGoMod returned ok=false")
	}
	if !strings.Contains(out, "myapp") {
		t.Errorf("output missing module name: %q", out)
	}
	if !strings.Contains(out, "1.21") {
		t.Errorf("output missing go version: %q", out)
	}
	if !strings.Contains(out, "foo/bar") {
		t.Errorf("output missing dep foo/bar: %q", out)
	}
	if !strings.Contains(out, "baz/qux") {
		t.Errorf("output missing indirect baz/qux: %q", out)
	}
}

func TestCompressGoModMissingModule(t *testing.T) {
	_, ok := compressGoMod([]byte("go 1.21\n"))
	if ok {
		t.Error("expected ok=false when module line missing")
	}
}

func TestCompressGoModSingleLineRequire(t *testing.T) {
	gomod := `module example.com/x

require github.com/single/dep v1.0.0
`
	out, ok := compressGoMod([]byte(gomod))
	if !ok {
		t.Fatal("compressGoMod returned ok=false")
	}
	if !strings.Contains(out, "single/dep") {
		t.Errorf("single-line require not parsed: %q", out)
	}
}

// ─── compressPkgJSON ──────────────────────────────────────────────────────────

func TestCompressPkgJSON(t *testing.T) {
	data := `{
  "name": "my-app",
  "version": "1.0.0",
  "dependencies": { "react": "^18.0.0", "lodash": "^4.0.0" },
  "devDependencies": { "jest": "^29.0.0" },
  "scripts": { "build": "tsc", "test": "jest" }
}`
	out, ok := compressPkgJSON([]byte(data))
	if !ok {
		t.Fatal("compressPkgJSON returned ok=false")
	}
	if !strings.Contains(out, "my-app") {
		t.Errorf("missing package name: %q", out)
	}
	if !strings.Contains(out, "react") {
		t.Errorf("missing dep react: %q", out)
	}
	if !strings.Contains(out, "jest") {
		t.Errorf("missing devDep jest: %q", out)
	}
}

func TestCompressPkgJSONInvalid(t *testing.T) {
	_, ok := compressPkgJSON([]byte("not json"))
	if ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

// ─── compressCargoToml ────────────────────────────────────────────────────────

func TestCompressCargoToml(t *testing.T) {
	data := `[package]
name = "mycrate"
version = "0.1.0"

[dependencies]
serde = "1.0"
tokio = { version = "1", features = ["full"] }

[dev-dependencies]
mockall = "0.11"
`
	out, ok := compressCargoToml([]byte(data))
	if !ok {
		t.Fatal("compressCargoToml returned ok=false")
	}
	if !strings.Contains(out, "mycrate") {
		t.Errorf("missing crate name: %q", out)
	}
	if !strings.Contains(out, "serde") {
		t.Errorf("missing dep serde: %q", out)
	}
	if !strings.Contains(out, "tokio") {
		t.Errorf("missing dep tokio: %q", out)
	}
}

func TestCompressCargoTomlMissingName(t *testing.T) {
	_, ok := compressCargoToml([]byte("[dependencies]\nfoo = \"1.0\"\n"))
	if ok {
		t.Error("expected ok=false when crate name missing")
	}
}

// ─── compressRequirementsTxt ──────────────────────────────────────────────────

func TestCompressRequirementsTxt(t *testing.T) {
	data := `requests==2.31.0
flask>=2.0.0
# comment line
pytest==7.4.0  # dev
-r other.txt
`
	out, ok := compressRequirementsTxt("requirements.txt", []byte(data))
	if !ok {
		t.Fatal("compressRequirementsTxt returned ok=false")
	}
	if !strings.Contains(out, "requests") {
		t.Errorf("missing requests: %q", out)
	}
	if !strings.Contains(out, "flask") {
		t.Errorf("missing flask: %q", out)
	}
}

func TestCompressRequirementsTxtEmpty(t *testing.T) {
	_, ok := compressRequirementsTxt("requirements.txt", []byte("# only comments\n"))
	if ok {
		t.Error("expected ok=false for requirements with no packages")
	}
}

// ─── compressPyprojectToml ────────────────────────────────────────────────────

func TestCompressPyprojectToml(t *testing.T) {
	// project name from [project], deps from [tool.poetry.dependencies].
	data := `[project]
name = "mypackage"
version = "0.1.0"

[tool.poetry.dependencies]
requests = "^2.28"
click = "^8.0"

[tool.poetry.dev-dependencies]
pytest = "^7.0"
`
	out, ok := compressPyprojectToml([]byte(data))
	if !ok {
		t.Fatal("compressPyprojectToml returned ok=false")
	}
	if !strings.Contains(out, "mypackage") {
		t.Errorf("missing project name: %q", out)
	}
	if !strings.Contains(out, "requests") {
		t.Errorf("missing dep requests: %q", out)
	}
}

func TestCompressPyprojectTomlMissingName(t *testing.T) {
	// ok=false only when both name and deps are empty.
	_, ok := compressPyprojectToml([]byte("[project]\n"))
	if ok {
		t.Error("expected ok=false when both name and deps are absent")
	}
}

// ─── compressGemfile ──────────────────────────────────────────────────────────

func TestCompressGemfile(t *testing.T) {
	data := `source 'https://rubygems.org'

gem 'rails', '~> 7.0'
gem 'pg', '>= 0.18'

group :development, :test do
  gem 'rspec-rails'
  gem 'factory_bot_rails'
end
`
	out, ok := compressGemfile([]byte(data))
	if !ok {
		t.Fatal("compressGemfile returned ok=false")
	}
	if !strings.Contains(out, "rails") {
		t.Errorf("missing gem rails: %q", out)
	}
	if !strings.Contains(out, "pg") {
		t.Errorf("missing gem pg: %q", out)
	}
}

func TestCompressGemfileEmpty(t *testing.T) {
	_, ok := compressGemfile([]byte("source 'https://rubygems.org'\n"))
	if ok {
		t.Error("expected ok=false when no gems declared")
	}
}

// ─── compressDepsFile (dispatch) ──────────────────────────────────────────────

func TestCompressDepsFileDispatch(t *testing.T) {
	cases := []struct {
		path    string
		data    string
		wantOK  bool
		contain string
	}{
		{
			path:    "path/to/go.mod",
			data:    "module example.com/x\n\ngo 1.21\n",
			wantOK:  true,
			contain: "example.com/x",
		},
		{
			path:    "package.json",
			data:    `{"name":"app","dependencies":{"react":"18"}}`,
			wantOK:  true,
			contain: "app",
		},
		{
			path:   "unknown.txt",
			wantOK: false,
		},
		{
			path:   "go.sum",
			wantOK: false, // go.sum falls through to default
		},
	}
	for _, c := range cases {
		out, ok := compressDepsFile(c.path, []byte(c.data))
		if ok != c.wantOK {
			t.Errorf("compressDepsFile(%q): ok=%v, want %v (out=%q)", c.path, ok, c.wantOK, out)
			continue
		}
		if c.contain != "" && !strings.Contains(out, c.contain) {
			t.Errorf("compressDepsFile(%q) output missing %q: %q", c.path, c.contain, out)
		}
	}
}

// ─── shortPkg ─────────────────────────────────────────────────────────────────

func TestShortPkg(t *testing.T) {
	cases := []struct{ pkg, want string }{
		{"github.com/foo/bar", "foo/bar"},
		{"github.com/org/repo/sub", "repo/sub"},
		{"simple", "simple"},
		{"two/parts", "two/parts"},
	}
	for _, c := range cases {
		if got := shortPkg(c.pkg); got != c.want {
			t.Errorf("shortPkg(%q) = %q, want %q", c.pkg, got, c.want)
		}
	}
}
