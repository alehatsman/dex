package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

// cmdVerify runs the tests a change implicates and reports pass/fail (MCP:
// verify). With no selector it tests the uncommitted working tree; --ref tests
// a git range; --symbol tests a symbol's blast-radius. It closes the
// change→verify→learn loop: a failing run surfaces a gotcha candidate (#686).
func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	setHelp(fs,
		"Run the tests a change implicates and report pass/fail (MCP: verify). Go-only in v1.",
		"dex verify [flags] [<path>]",
		"dex verify                          # test uncommitted working-tree changes",
		"dex verify --ref HEAD~3..HEAD       # test what a range changed",
		"dex verify --symbol NewServer       # test a symbol's blast-radius")
	ref := fs.String("ref", "", "git range/ref to test instead of the working tree")
	symbol := fs.String("symbol", "", "test a symbol's blast-radius instead of a diff")
	command := fs.String("command", "", "override the test command template ({{packages}} placeholder)")
	timeout := fs.Int("timeout", 0, "test-run timeout seconds (default 60, max 600)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("verify takes no positional args besides [<path>] — use --symbol or --ref (got %d extra)", len(rest))
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.Verify(ctx, mcp.VerifyInput{
		Symbol:      *symbol,
		Ref:         *ref,
		Command:     *command,
		TimeoutSecs: *timeout,
		ProjectRoot: p.Root,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		// Mirror text-mode exit semantics: a completed run that did not
		// pass exits non-zero so JSON consumers (CI/agents) can gate on it.
		if verifyFailed(out.Status, out.Passed) {
			os.Exit(1)
		}
		return nil
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return nil
	}
	result := "PASS"
	if !out.Passed {
		result = "FAIL"
	}
	fmt.Printf("%s (%s) — %d package(s)\n$ %s\n\n", result, out.Mode, len(out.Packages), out.Command)
	fmt.Print(out.Output)
	if !out.Passed && out.GotchaCandidate != nil {
		fmt.Fprintf(os.Stderr, "\ngotcha candidate [%s]: %s\n", out.GotchaCandidate.Trigger, out.GotchaCandidate.OutputFragment)
	}
	if verifyFailed(out.Status, out.Passed) {
		os.Exit(1)
	}
	return nil
}

// verifyFailed reports whether a completed verify run counts as a failure
// (drives the non-zero exit in both text and JSON render paths). A run whose
// status is not "ok" did not complete and is reported without a failure exit.
func verifyFailed(status string, passed bool) bool {
	return status == "ok" && !passed
}
