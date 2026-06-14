package compress

import "strings"

// Compressor handles command-output compression for a family of commands.
// Match reports whether cmd (already lower-cased and trimmed) belongs to the
// family; Compress transforms the output lines and receives the full cmd so a
// handler can vary behaviour by subcommand. A Compress that returns nil
// declines the output and Dispatch falls through to the next registered
// compressor — preserving the original "first non-nil result wins" routing.
type Compressor struct {
	// Name identifies the compressor in tests and diagnostics.
	Name string
	// Match reports whether this compressor handles cmd.
	Match func(cmd string) bool
	// Compress transforms the output lines, or returns nil to decline.
	Compress func(cmd string, lines []string) []string
}

// Register appends c to the dispatch table at the lowest precedence. The
// built-in compressors are seeded into the table below; call Register to add
// one without editing that table (e.g. from a test or an out-of-tree build).
// Registration order is dispatch precedence — register before the first
// Dispatch call.
func Register(c Compressor) { registry = append(registry, c) }

// Dispatch routes cmd's output through the registered compressors in order and
// returns the first non-nil result, falling back to generic log patterns when
// none claim it.
func Dispatch(cmd string, lines []string) []string {
	for i := range registry {
		if registry[i].Match(cmd) {
			if out := registry[i].Compress(cmd, lines); out != nil {
				return out
			}
		}
	}
	return dispatchDefault(lines)
}

// dispatchDefault is the terminal fallback when no registered compressor
// claims the output: log block-folding, then dedup, then a generic squeeze.
func dispatchDefault(lines []string) []string {
	if blocked := CompressLogBlock(lines); blocked != nil {
		return blocked
	}
	if dedupd := CompressLogDedup(lines); dedupd != nil {
		return dedupd
	}
	return CompressGeneric(lines)
}

// ── match helpers ─────────────────────────────────────────────────────────────

// linesOnly adapts a line-only compressor to the cmd-aware Compressor shape.
func linesOnly(fn func([]string) []string) func(string, []string) []string {
	return func(_ string, lines []string) []string { return fn(lines) }
}

// hasPrefix reports whether cmd starts with any of the given prefixes.
func hasPrefix(ps ...string) func(string) bool {
	return func(cmd string) bool {
		for _, p := range ps {
			if strings.HasPrefix(cmd, p) {
				return true
			}
		}
		return false
	}
}

// word matches cmd equal to w or beginning with "w " — a word-bounded prefix
// that distinguishes e.g. "ps" from "psql".
func word(w string) func(string) bool {
	return func(cmd string) bool {
		return cmd == w || strings.HasPrefix(cmd, w+" ")
	}
}

// or combines predicates: cmd matches if any does.
func or(fns ...func(string) bool) func(string) bool {
	return func(cmd string) bool {
		for _, f := range fns {
			if f(cmd) {
				return true
			}
		}
		return false
	}
}

// ── built-in compressors ───────────────────────────────────────────────────────
//
// The table is the registration interface: a new language or tool adds one
// entry here (or calls Register) instead of editing a dispatch switch. Order
// is precedence; it mirrors the previous category-grouped switch exactly.
var registry = []Compressor{
	// build tools
	{Name: "go-test", Match: hasPrefix("go test"), Compress: linesOnly(CompressGoTest)},
	{Name: "go-build", Match: hasPrefix("go build", "go vet"), Compress: linesOnly(CompressGoBuild)},
	{Name: "cargo", Match: hasPrefix("cargo"), Compress: linesOnly(CompressCargo)},
	{Name: "cmake", Match: hasPrefix("cmake", "ninja", "gcc ", "g++ ", "cc "), Compress: linesOnly(CompressCmake)},
	{Name: "bazel", Match: hasPrefix("bazel "), Compress: CompressBazel},
	{Name: "maven", Match: hasPrefix("mvn ", "./mvnw ", "mvnw ", "gradle ", "./gradlew ", "gradlew "), Compress: CompressMaven},
	{Name: "swift", Match: hasPrefix("swift "), Compress: CompressSwiftBuild},
	{Name: "zig", Match: hasPrefix("zig "), Compress: CompressZig},

	// infra
	{Name: "git", Match: hasPrefix("git"), Compress: linesOnly(CompressGit)},
	{Name: "gh", Match: hasPrefix("gh "), Compress: linesOnly(CompressGh)},
	{Name: "docker", Match: hasPrefix("docker"), Compress: linesOnly(CompressDocker)},
	{Name: "kubectl", Match: hasPrefix("kubectl"), Compress: linesOnly(CompressKubectl)},
	{Name: "terraform", Match: hasPrefix("terraform", "tofu"), Compress: linesOnly(CompressTerraform)},
	{Name: "helm", Match: hasPrefix("helm "), Compress: CompressHelm},
	{Name: "ansible", Match: hasPrefix("ansible", "ansible-playbook"), Compress: linesOnly(CompressAnsible)},
	{Name: "make", Match: hasPrefix("make", "gmake"), Compress: linesOnly(CompressMake)},

	// js / ts tools
	{Name: "npm", Match: hasPrefix("npm ", "yarn ", "bun ", "pnpm ", "turbo ", "nx "), Compress: linesOnly(CompressNpm)},
	{Name: "eslint", Match: hasPrefix("eslint", "npx eslint", "biome", "hadolint", "yamllint", "markdownlint", "oxlint"), Compress: linesOnly(CompressEslint)},
	{Name: "tsc", Match: hasPrefix("tsc", "npx tsc"), Compress: linesOnly(CompressTsc)},
	{Name: "playwright", Match: hasPrefix("npx playwright", "playwright", "npx cypress", "cypress"), Compress: CompressPlaywright},
	{Name: "next-build", Match: hasPrefix("next build", "npx next build", "vite build", "npx vite build"), Compress: CompressNextBuild},
	{Name: "prettier", Match: hasPrefix("prettier ", "npx prettier "), Compress: linesOnly(CompressPrettier)},
	{Name: "prisma", Match: hasPrefix("npx prisma ", "prisma "), Compress: CompressPrisma},

	// python tools
	{Name: "pip", Match: hasPrefix("pip ", "pip3 ", "uv ", "conda ", "mamba ", "pipx "), Compress: linesOnly(CompressPip)},
	{Name: "ruff", Match: hasPrefix("ruff"), Compress: CompressRuff},
	{Name: "mypy", Match: hasPrefix("mypy", "pyright", "basedpyright"), Compress: linesOnly(CompressMypy)},
	{Name: "pytest", Match: hasPrefix("pytest", "python -m pytest", "python3 -m pytest", "vitest", "jest", "mocha", "jasmine"), Compress: linesOnly(CompressPytest)},

	// language package managers
	{Name: "poetry", Match: hasPrefix("poetry "), Compress: CompressPoetry},
	{Name: "ruby", Match: hasPrefix("rubocop", "bundle ", "rake ", "rails "), Compress: CompressRuby},
	{Name: "composer", Match: hasPrefix("composer "), Compress: CompressComposer},
	{Name: "artisan", Match: hasPrefix("php artisan "), Compress: func(cmd string, lines []string) []string {
		return CompressArtisan(cmd[4:], lines) // strip the "php " prefix
	}},
	{Name: "mix", Match: hasPrefix("mix "), Compress: CompressMix},

	// system tools
	{Name: "grep", Match: hasPrefix("grep ", "rg ", "ag ", "ack "), Compress: linesOnly(CompressGrep)},
	{Name: "find", Match: hasPrefix("find ", "fd "), Compress: linesOnly(CompressFind)},
	{Name: "ps", Match: word("ps"), Compress: linesOnly(CompressPs)},
	{Name: "du", Match: word("du"), Compress: linesOnly(CompressDu)},
	{Name: "ping", Match: hasPrefix("ping "), Compress: linesOnly(CompressPing)},
	{Name: "systemd", Match: or(word("systemctl"), hasPrefix("journalctl")), Compress: CompressSystemd},

	// misc
	{
		Name: "deps-file",
		Match: func(cmd string) bool {
			return hasPrefix("cat ", "bat ", "batcat ")(cmd) && IsDepsFilename(DepsFileArg(cmd))
		},
		Compress: func(cmd string, lines []string) []string {
			if summary, ok := CompressDepsFile(DepsFileArg(cmd), []byte(strings.Join(lines, "\n"))); ok {
				return strings.Split(summary, "\n")
			}
			return nil
		},
	},
	{Name: "ls", Match: word("ls"), Compress: linesOnly(CompressLs)},
	{Name: "mysql", Match: or(word("mysql"), hasPrefix("mariadb ")), Compress: CompressMySQL},
	{Name: "psql", Match: word("psql"), Compress: CompressPsql},
	{Name: "env", Match: or(hasPrefix("env"), word("printenv"), word("export")), Compress: linesOnly(CompressEnvFilter)},
}
