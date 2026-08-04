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

// SynthVerifyCommand builds the test command from the override, else
// $DEX_VERIFY_CMD, else the default. A "{{packages}}" placeholder is replaced
// with the space-joined package list; without it the list is appended.
func SynthVerifyCommand(override string, pkgs []string) string {
	tmpl := strings.TrimSpace(override)
	if tmpl == "" {
		tmpl = strings.TrimSpace(os.Getenv("DEX_VERIFY_CMD"))
	}
	if tmpl == "" {
		tmpl = "go test {{packages}}"
	}
	joined := strings.Join(pkgs, " ")
	if strings.Contains(tmpl, "{{packages}}") {
		return strings.ReplaceAll(tmpl, "{{packages}}", joined)
	}
	return tmpl + " " + joined
}
