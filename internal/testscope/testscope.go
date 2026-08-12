// Package testscope turns the set of files a change implicates into the runnable
// test scope for the `verify` verb (#111): the Go package list, the sibling test
// files, and the synthesized test command. Pure over its inputs — the mcp
// transport owns resolving the changed set (git diff / graph impact) and running
// the command through the shell pipeline.
package testscope

import (
	"os"
	"path"
	"sort"
	"strings"
)

// GoPackagesForFiles reduces a file list to the sorted, de-duplicated set of Go
// package directories, as `go test` patterns ("./internal/mcp", "."). Non-.go
// files are dropped (verify is Go-only in v1).
func GoPackagesForFiles(files []string) []string {
	seen := map[string]bool{}
	var pkgs []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		dir := path.Dir(f)
		pkg := "."
		if dir != "." && dir != "" {
			pkg = "./" + dir
		}
		if !seen[pkg] {
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	sort.Strings(pkgs)
	return pkgs
}

// SiblingTestFiles is the informational list of test files for the changed set
// (foo.go ↔ foo_test.go). Best-effort: it does not check existence.
func SiblingTestFiles(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		tf := f
		if !strings.HasSuffix(f, "_test.go") {
			tf = strings.TrimSuffix(f, ".go") + "_test.go"
		}
		if !seen[tf] {
			seen[tf] = true
			out = append(out, tf)
		}
	}
	sort.Strings(out)
	return out
}

// SynthVerifyCommand resolves the test command in precedence order: the explicit
// override → $DEX_VERIFY_CMD → the repo's declared canonical test command (#146,
// discovered by the caller) → the go-test default.
//
// Override and env are user-authored templates: a "{{packages}}" placeholder is
// replaced with the space-joined package list, else the list is appended.
//
// canonical is the project's own test command (e.g. "mooncake task test", "go test
// ./..."). A whole-module `go test … ./...` (or trailing ".") canonical is
// re-scoped to the implicated packages, preserving its flags — so a plain-Go
// repo still gets fast, targeted runs. Any other canonical (a task runner) runs
// verbatim: appending package paths to "mooncake task test" would be nonsense.
func SynthVerifyCommand(override, canonical string, pkgs []string) string {
	joined := strings.Join(pkgs, " ")
	if t := strings.TrimSpace(override); t != "" {
		return applyTemplate(t, joined)
	}
	if t := strings.TrimSpace(os.Getenv("DEX_VERIFY_CMD")); t != "" {
		return applyTemplate(t, joined)
	}
	if t := strings.TrimSpace(canonical); t != "" {
		if scoped, ok := scopeGoTest(t, joined); ok {
			return scoped
		}
		return t // task runner — run its declared command verbatim
	}
	return applyTemplate("go test {{packages}}", joined)
}

// applyTemplate substitutes a "{{packages}}" placeholder with joined, or appends
// joined when the template omits it — the user-template convenience.
func applyTemplate(tmpl, joined string) string {
	if strings.Contains(tmpl, "{{packages}}") {
		return strings.ReplaceAll(tmpl, "{{packages}}", joined)
	}
	return strings.TrimSpace(tmpl + " " + joined)
}

// scopeGoTest re-scopes a whole-module `go test … ./...` (or trailing ".")
// invocation to the implicated package list, preserving the flags in between.
// ok is false for anything that isn't a go-test whole-module form (task runners,
// go test already targeting specific packages) — the caller runs those verbatim.
func scopeGoTest(cmd, joined string) (string, bool) {
	fields := strings.Fields(cmd)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "test" {
		return "", false
	}
	last := fields[len(fields)-1]
	if last != "./..." && last != "." {
		return "", false
	}
	if joined == "" {
		return cmd, true // nothing implicated — leave the whole-module target
	}
	return strings.Join(fields[:len(fields)-1], " ") + " " + joined, true
}
