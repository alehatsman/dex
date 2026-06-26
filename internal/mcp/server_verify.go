package mcp

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/gotcha"
	"github.com/alehatsman/dex/internal/review"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// verify (#686, epic #683) closes the agent loop's missing half: change →
// verify → learn. It resolves the tests a change implicates, runs them through
// the shell pipeline (so output is compressed and a failing run stages a
// gotcha_candidate, #601/#616), and returns pass/fail in one call.
//
// Every lane it composes already ships in dex: graphImpact computes a symbol's
// blast-radius tests (#654), gitDiffUnified + review.ParseUnified turn a diff
// into changed files, and shellRun runs the command + stages the gotcha. verify
// is the wiring that makes them one call.
//
// It is the first non-read-only query verb — it runs the test command — so it
// is registered without ReadOnlyHint.

// VerifyInput drives the verify verb. Mode is chosen most-specific-first:
//   - Symbol set → the blast-radius tests of that symbol.
//   - Ref set    → the tests of the files a git range changed.
//   - neither    → the tests of the uncommitted working-tree changes (vs HEAD).
type VerifyInput struct {
	Symbol      string `json:"symbol,omitempty" jsonschema:"test a symbol's blast-radius (its own test plus its callers'); bare ('Foo'), receiver-qualified ('(*Server).Run'), or package-tail-qualified ('mcp.NewServer')"`
	Ref         string `json:"ref,omitempty" jsonschema:"git range or ref to test the changes of (e.g. 'HEAD~3..HEAD'); default (no symbol, no ref) tests the uncommitted working-tree changes vs HEAD"`
	Command     string `json:"command,omitempty" jsonschema:"override the test command template; '{{packages}}' is substituted with the resolved Go package list (e.g. 'go test -tags sqlite_fts5 {{packages}}'). Falls back to $DEX_VERIFY_CMD then 'go test {{packages}}'"`
	TimeoutSecs int    `json:"timeout_secs,omitempty" jsonschema:"test-run timeout in seconds (default 60, max 600)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// VerifyOutput carries the resolved test scope and the run result. On a failing
// run GotchaCandidate is set when shellRun recognized the failure signature —
// the agent persists it with `notes add`.
type VerifyOutput struct {
	Status          string            `json:"status"` // "ok" | "no-tests" | "no-changes" | "no-index" | "no-graph" | "not-found" | "error"
	Hint            string            `json:"hint,omitempty"`
	Project         string            `json:"project,omitempty"`
	Mode            string            `json:"mode,omitempty"` // "worktree" | "ref" | "symbol"
	Packages        []string          `json:"packages,omitempty"`
	Tests           []string          `json:"tests,omitempty"` // sibling test files implicated (informational)
	Command         string            `json:"command,omitempty"`
	ExitCode        int               `json:"exit_code"`
	Passed          bool              `json:"passed"`
	Output          string            `json:"output,omitempty"`
	SavedPct        int               `json:"saved_pct,omitempty"`
	GotchaCandidate *gotcha.Candidate `json:"gotcha_candidate,omitempty"`
}

// Verify runs the verify verb without an SDK request — the REST `/verify` route
// and the `dex verify` CLI. It composes over the local *Server exactly like the
// stdio `verify` tool, so all transports agree.
func (s *Server) Verify(ctx context.Context, in VerifyInput) (VerifyOutput, error) {
	_, out, err := s.verify(ctx, nil, in)
	return out, err
}

func (s *Server) verify(ctx context.Context, req *sdk.CallToolRequest, in VerifyInput) (*sdk.CallToolResult, VerifyOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, VerifyOutput{Status: "error", Hint: hint}, nil
	}

	mode, files, early := s.verifyResolve(ctx, req, p.Root, in)
	if early != nil {
		early.Project = p.Root
		early.Mode = mode
		return nil, *early, nil
	}

	pkgs := goPackagesForFiles(files)
	if len(pkgs) == 0 {
		return nil, VerifyOutput{Status: "no-tests", Project: p.Root, Mode: mode,
			Hint: "no Go package is implicated by the change — verify is Go-only in v1"}, nil
	}

	cmd := synthVerifyCommand(in.Command, pkgs)
	_, sh, err := s.shellRun(ctx, req, ShellInput{Command: cmd, Cwd: p.Root, TimeoutSecs: in.TimeoutSecs})
	if err != nil {
		return nil, VerifyOutput{Status: "error", Project: p.Root, Mode: mode, Command: cmd,
			Hint: fmt.Sprintf("run tests: %v", err)}, nil
	}

	return nil, VerifyOutput{
		Status:          "ok",
		Project:         p.Root,
		Mode:            mode,
		Packages:        pkgs,
		Tests:           siblingTestFiles(files),
		Command:         cmd,
		ExitCode:        sh.ExitCode,
		Passed:          sh.ExitCode == 0,
		Output:          sh.Output,
		SavedPct:        sh.SavedPct,
		GotchaCandidate: sh.GotchaCandidate,
	}, nil
}

// verifyResolve picks the mode from the inputs and returns the implicated Go
// files. early is non-nil for a terminal status (no-changes/no-graph/error)
// that the caller returns verbatim after stamping Project+Mode.
func (s *Server) verifyResolve(ctx context.Context, req *sdk.CallToolRequest, root string, in VerifyInput) (mode string, files []string, early *VerifyOutput) {
	if sym := strings.TrimSpace(in.Symbol); sym != "" {
		_, imp, err := s.graphImpact(ctx, req, ImpactInput{Name: sym, ProjectRoot: root})
		if err != nil {
			return "symbol", nil, &VerifyOutput{Status: "error", Hint: err.Error()}
		}
		if imp.Status != "ok" {
			return "symbol", nil, &VerifyOutput{Status: imp.Status, Hint: imp.Hint}
		}
		return "symbol", impactFiles(imp), nil
	}

	rng, mode := "HEAD", "worktree"
	if r := strings.TrimSpace(in.Ref); r != "" {
		if !reValidRef.MatchString(r) {
			return "ref", nil, &VerifyOutput{Status: "error", Hint: fmt.Sprintf("invalid git ref/range %q", r)}
		}
		rng, mode = r, "ref"
	}
	diff, err := gitDiffUnified(ctx, root, rng)
	if err != nil {
		return mode, nil, &VerifyOutput{Status: "error",
			Hint: fmt.Sprintf("git diff %q failed — check it is a valid ref (try `git rev-parse %s`)", rng, rng)}
	}
	fds := review.ParseUnified(diff)
	if len(fds) == 0 {
		return mode, nil, &VerifyOutput{Status: "no-changes", Hint: fmt.Sprintf("no changes in %s", rng)}
	}
	for _, fd := range fds {
		if fd.Status == "deleted" {
			continue // a deleted file's package may still need testing, but its own path is gone
		}
		files = append(files, fd.Path)
	}
	return mode, files, nil
}

// impactFiles gathers the .go files an impact result implicates: the target's
// own files, its reachable callers, and the sibling tests already computed for
// the blast radius (#654).
func impactFiles(imp ImpactOutput) []string {
	var files []string
	for _, t := range imp.Targets {
		files = append(files, t.Path)
	}
	for _, n := range imp.Nodes {
		files = append(files, n.Path)
	}
	files = append(files, imp.TestsToRun...)
	return files
}

// goPackagesForFiles reduces a file list to the sorted, de-duplicated set of Go
// package directories, as `go test` patterns ("./internal/mcp", "."). Non-.go
// files are dropped (verify is Go-only in v1).
func goPackagesForFiles(files []string) []string {
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

// siblingTestFiles is the informational list of test files for the changed set
// (foo.go ↔ foo_test.go). Best-effort: it does not check existence.
func siblingTestFiles(files []string) []string {
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

// synthVerifyCommand builds the test command from the override, else
// $DEX_VERIFY_CMD, else the default. A "{{packages}}" placeholder is replaced
// with the space-joined package list; without it the list is appended.
func synthVerifyCommand(override string, pkgs []string) string {
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
