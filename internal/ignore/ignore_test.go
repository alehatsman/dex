package ignore

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultsIgnoreVendorDirs(t *testing.T) {
	root := t.TempDir()
	// Include everything so this exercises the exclude layer in isolation
	// (without an include list the opt-in model would skip every file).
	writeConfig(t, root, "index:\n  include: [\"*\"]\n")
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{"node_modules", true, true},
		{"node_modules/foo.js", false, true},
		{"vendor/bar/baz.go", false, true},
		{".git", true, true},
		{".git/HEAD", false, true},
		{"dist", true, true},
		{"build/out.txt", false, true},
		{".env", false, true},
		{".env.local", false, true},
		{"id_rsa", false, true},
		{"id_ed25519.pub", false, true},
		{"secrets.yml", false, true},
		{"foo.min.js", false, true},
		// generated / aggregated artifacts that slip past the
		// extension allow-list (.ts / .txt / .json / .yaml).
		{"types/index.d.ts", false, true},
		{"foo.d.ts", false, true},
		{"llms.txt", false, true},
		{"llms-full.txt", false, true},
		{"package-lock.json", false, true},
		{"npm-shrinkwrap.json", false, true},
		{"pnpm-lock.yaml", false, true},
		{"coverage/lcov.info", false, true},
		{".nyc_output/out.json", false, true},
		{"htmlcov/index.html", false, true},
		{"src/__snapshots__/App.test.tsx.snap", false, true},
		// license-family files: ignored at the gitignore layer so
		// their uniform legalese can't pollute RAG.
		{"LICENSE", false, true},
		{"LICENSE.md", false, true},
		{"LICENSE.txt", false, true},
		{"LICENCE", false, true},
		{"License.md", false, true},
		{"license.txt", false, true},
		{"COPYING", false, true},
		{"COPYING.LESSER", false, true},
		{"COPYRIGHT", false, true},
		{"NOTICE", false, true},
		{"NOTICE.md", false, true},
		{"AUTHORS", false, true},
		{"PATENTS", false, true},
		{"LEGAL.md", false, true},
		// negatives
		{"src/main.go", false, false},
		{"README.md", false, false},
		{"CHANGELOG.md", false, false},
		{".github/workflows/ci.yml", false, false},
		// hand-written TS/JSON/txt must stay indexable — *.d.ts and the
		// lockfile/llms patterns must not over-match.
		{"src/main.ts", false, false},
		{"config.json", false, false},
		{"notes.txt", false, false},
	}
	for _, c := range cases {
		got := m.Match(c.path, c.isDir)
		if got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestDefaultBuildPatternsAnchoredToRoot(t *testing.T) {
	// Generic-word build-output dirs must be ignored at the repo root but
	// NOT at any depth — a source package named build/ dist/ target/ etc.
	// must stay indexable (#457). Tool-specific dirs (node_modules, vendor)
	// stay unanchored and are excluded wherever they appear.
	root := t.TempDir()
	writeConfig(t, root, "index:\n  include: [\"*\"]\n")
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		// root-level build outputs: still ignored
		{"build", true, true},
		{"dist", true, true},
		{"target", true, true},
		{"coverage", true, true},
		{"htmlcov", true, true},
		{".next", true, true},
		{".cache", true, true},
		{"build/out.o", false, true},
		{"target/release/app", false, true},
		// same names nested as source packages: must be indexed now
		{"internal/build", true, false},
		{"internal/build/server.go", false, false},
		{"pkg/dist/client.go", false, false},
		{"cmd/target/main.go", false, false},
		{"src/coverage/report.go", false, false},
		{"app/htmlcov/render.go", false, false},
		// tool-specific dirs stay unanchored: excluded at any depth
		{"pkg/node_modules/foo.js", false, true},
		{"app/vendor/lib/util.go", false, true},
		{"svc/__pycache__/mod.py", false, true},
	}
	for _, c := range cases {
		if got := m.Match(c.path, c.isDir); got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestGitignoreAndMcsearchIgnore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"),
		[]byte("# project\n*.tmp\n/build\ndocs/private/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dexignore"),
		[]byte("scratch/\n!scratch/keep.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, "index:\n  include: [\"*\"]\n")
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{"foo.tmp", false, true},
		{"a/b/c.tmp", false, true},
		{"build", true, true},
		{"build/x", false, true},
		{"docs/private", true, true},
		{"docs/private/secret.md", false, true},
		{"docs/public/howto.md", false, false},
		{"scratch", true, true},
		{"scratch/x.md", false, true},
		// negation re-includes a specific file
		{"scratch/keep.md", false, false},
		{"src/main.go", false, false},
	}
	for _, c := range cases {
		got := m.Match(c.path, c.isDir)
		if got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestDoubleStarPatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".dexignore"),
		[]byte("**/__pycache__/\n**/*.bak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, "index:\n  include: [\"*\"]\n")
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		{"__pycache__", true, true},
		{"src/__pycache__", true, true},
		{"deep/a/b/__pycache__/foo.pyc", false, true},
		{"x.bak", false, true},
		{"deep/x.bak", false, true},
		{"src/main.py", false, false},
	}
	for _, c := range cases {
		got := m.Match(c.path, c.isDir)
		if got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestIndexableExt(t *testing.T) {
	cases := map[string]bool{
		"main.go":     true,
		"main.GO":     true, // case-insensitive
		"app.py":      true,
		"x.rs":        true,
		"README.md":   true,
		"y.unknown":   false,
		"binary":      false,
		"image.png":   false,
		"sub/dir/x.c": true,
		// extended language coverage
		"App.swift":      true,
		"Program.cs":     true,
		"build.scala":    true,
		"main.dart":      true,
		"index.php":      true,
		"Lib.hs":         true,
		"app.ex":         true,
		"app.exs":        true,
		"node.erl":       true,
		"main.tf":        true,
		"schema.proto":   true,
		"schema.graphql": true,
		"schema.gql":     true,
		"analysis.r":     true,
		"workflow.jl":    true,
		"main.zig":       true,
		"config.fish":    true,
		"deploy.ps1":     true,
		// markup / web
		"index.html": true,
		"style.css":  true,
		"theme.scss": true,
		"App.vue":    true,
		"App.svelte": true,
	}
	for path, want := range cases {
		if got := IndexableExt(path); got != want {
			t.Errorf("IndexableExt(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIndexableBasename(t *testing.T) {
	cases := map[string]bool{
		"Makefile":             true,
		"GNUmakefile":          true,
		"Dockerfile":           true,
		"Containerfile":        true,
		"sub/Dockerfile":       true,
		"x.go":                 false,
		"makefile":             true,
		"random":               false,
		"CMakeLists.txt":       true,
		"build/CMakeLists.txt": true,
		// Go modules
		"go.mod":  true,
		"go.work": true,
		// Ruby DSL-style / dev environment
		"Brewfile":    true,
		"Vagrantfile": true,
		"Tiltfile":    true,
		"Caddyfile":   true,
		"Pipfile":     true,
		// Editor
		".editorconfig": true,
		// Substantive docs without extension stay indexable.
		"CHANGELOG": true,
		"README":    true,
		// License-family files: the basename whitelist does NOT carry
		// them — they're filtered at the gitignore-pattern layer
		// (see TestDefaultsIgnoreVendorDirs). The whitelist is the
		// first gate, so they wouldn't reach the matcher anyway, but
		// being explicit about it here documents the contract.
		"LICENSE": false,
		"COPYING": false,
		"AUTHORS": false,
		"NOTICE":  false,
		"PATENTS": false,
		"LEGAL":   false,
		"go.sum":  false,
		"license": false, // case-sensitive on purpose
	}
	for path, want := range cases {
		if got := IndexableBasename(path); got != want {
			t.Errorf("IndexableBasename(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLooksLikeSecret(t *testing.T) {
	cases := []struct {
		blob string
		hit  bool
	}{
		{"// regular code\nfunc foo() {}", false},
		{"AWS_KEY=AKIA0123456789ABCDEF", true},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n", true},
		{"token=ghp_" + repeat("A", 36), true},
		{"token=AIza" + repeat("a", 35), true},
		{"key=sk_live_" + repeat("x", 30), true},
		{"key=glpat-" + repeat("x", 24), true},
		// Should NOT trigger: prefix lookalikes
		{"sk-but-short", false},
		{"BEGINPRIVATE KEY but not a real header", false},
		// Regression (#660): the un-anchored "sk-" rule matched mid-word, so a
		// hyphenated identifier (ta·sk-, di·sk-, ri·sk-) was flagged as a secret
		// and the whole file over-skipped from the index. Word-anchored now.
		{"deploy the task-management-system-deployment-pipeline-config", false},
		{"the disk-usage-monitor-service-handler-module lives here", false},
		// A real OpenAI/Anthropic key (at a word boundary) still detected.
		{"OPENAI_API_KEY=sk-proj-" + repeat("a", 30), true},
	}
	for _, c := range cases {
		got := LooksLikeSecret([]byte(c.blob))
		if got != c.hit {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", trim(c.blob), got, c.hit)
		}
	}
}

// TestRedactSecretTokens_WordBoundary guards the shell-output redaction path
// (compress.RedactSecrets → ignore.RedactSecretTokens): a hyphenated word must
// survive byte-for-byte, while a real key is masked (#660).
func TestRedactSecretTokens_WordBoundary(t *testing.T) {
	clean := "run the task-management-system-deployment-pipeline now"
	if got := RedactSecretTokens(clean); got != clean {
		t.Errorf("hyphenated word mangled: %q -> %q", clean, got)
	}
	secret := "key sk-proj-" + repeat("z", 30)
	if got := RedactSecretTokens(secret); strings.Contains(got, "sk-proj-"+repeat("z", 30)) {
		t.Errorf("real key survived redaction: %q -> %q", secret, got)
	}
}

func TestIsTestPath(t *testing.T) {
	cases := map[string]bool{
		// Go
		"internal/ignore/ignore_test.go": true,
		"main.go":                        false,
		// Python
		"tests/test_auth.py":  true,
		"src/test_helpers.py": true,
		"src/auth_test.py":    true,
		"src/auth.py":         false,
		// JS/TS
		"src/foo.test.js":   true,
		"src/foo.spec.ts":   true,
		"src/foo.ts":        false,
		"__tests__/util.ts": true,
		// Rust
		"tests/integration.rs": true,
		"src/util_test.rs":     true,
		"src/util.rs":          false,
		// Ruby
		"spec/models/user_spec.rb": true,
		"app/models/user.rb":       false,
		// Generic fixture dirs
		"testdata/sample.json": true,
		"fixtures/keys.txt":    true,
	}
	for path, want := range cases {
		if got := IsTestPath(path); got != want {
			t.Errorf("IsTestPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLooksBinary(t *testing.T) {
	if LooksBinary([]byte("hello world")) {
		t.Error("plain text flagged as binary")
	}
	if !LooksBinary([]byte("hello\x00world")) {
		t.Error("NUL byte not detected")
	}
	// 8 KB scanning window — content after should not affect detection.
	big := make([]byte, 16384)
	for i := range big {
		big[i] = 'a'
	}
	big[9000] = 0
	if LooksBinary(big) {
		t.Error("NUL past 8 KB window should be ignored (false positive)")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

func trim(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// writeConfig writes a .dex/config.yml under root.
func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".dex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIncludeAllowList(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `# dex config
index:
  include:
    - cmd/
    - internal/
    - "*.md"
  ignore:
    - internal/legacy/
`)
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IncludeConfigured() {
		t.Fatal("IncludeConfigured() = false, want true")
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		// included trees
		{"cmd/dex/main.go", false, false},
		{"internal/ignore/ignore.go", false, false},
		{"README.md", false, false},
		{"docs/deep/notes.md", false, false}, // *.md matches at any depth
		// not in include → skipped even though they're ordinary source
		{"scripts/build.sh", false, true},
		{"go.mod", false, true},
		// ignore carve-out wins inside an included tree
		{"internal/legacy/old.go", false, true},
		{"internal/legacy", true, true},
		// directories are NOT filtered by include — the walk must still
		// descend into them to reach included files below.
		{"scripts", true, false},
		{"docs", true, false},
		// exclude set still applies, before include
		{"node_modules", true, true},
		{"node_modules/foo.js", false, true},
		{"internal/x.min.js", false, true},
	}
	for _, c := range cases {
		if got := m.Match(c.path, c.isDir); got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestNoIncludeIndexesNothing(t *testing.T) {
	root := t.TempDir() // no .dex/config.yml
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.IncludeConfigured() {
		t.Fatal("IncludeConfigured() = true, want false")
	}
	cases := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		// every file is skipped without an include list
		{"cmd/main.go", false, true},
		{"README.md", false, true},
		{"internal/store/store.go", false, true},
		// but directories are still walked, so files added to a future
		// include list remain reachable
		{"cmd", true, false},
		{"internal", true, false},
		// exclude set still prunes vendor dirs entirely
		{"node_modules", true, true},
		{"node_modules/x.js", false, true},
	}
	for _, c := range cases {
		if got := m.Match(c.path, c.isDir); got != c.ignored {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", c.path, c.isDir, got, c.ignored)
		}
	}
}

func TestLoadIndexConfig(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, root, `# leading comment
other:
  key: ignored

index:
  include:
    - cmd/        # trailing comment
    - internal/
    - "*.md"
  ignore: ["testdata/", "benchmark/results/"]
  scalar: skipped quietly
`)
	cfg, err := loadIndexConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	wantInc := []string{"cmd/", "internal/", "*.md"}
	wantIgn := []string{"testdata/", "benchmark/results/"}
	if !slices.Equal(cfg.Include, wantInc) {
		t.Errorf("Include = %v, want %v", cfg.Include, wantInc)
	}
	if !slices.Equal(cfg.Ignore, wantIgn) {
		t.Errorf("Ignore = %v, want %v", cfg.Ignore, wantIgn)
	}
}

func TestLoadIndexConfigEdgeCases(t *testing.T) {
	// Missing file → zero value, no error.
	cfg, err := loadIndexConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Include != nil || cfg.Ignore != nil {
		t.Errorf("missing file: got %+v, want zero", cfg)
	}

	// Flow-style sequence + empty sequence.
	root := t.TempDir()
	writeConfig(t, root, "index:\n  include: [\"a/\", \"b/\"]\n  ignore: []\n")
	cfg, err = loadIndexConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Include, []string{"a/", "b/"}) {
		t.Errorf("Include = %v, want [a/ b/]", cfg.Include)
	}
	if len(cfg.Ignore) != 0 {
		t.Errorf("Ignore = %v, want empty", cfg.Ignore)
	}

	// Malformed YAML → error.
	bad := t.TempDir()
	writeConfig(t, bad, "index:\n  include: [\n  \"a/\",\n")
	if _, err := loadIndexConfig(bad); err == nil {
		t.Error("malformed yaml: want error, got nil")
	}
}
