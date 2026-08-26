package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

// reLocateRef matches a 'path:line' / 'path:line:col' target so the positional
// argument can be auto-routed to the ref lane; anything else is a symbol.
var reLocateRef = regexp.MustCompile(`:\d+(:\d+)?$`)

// cmdLocate is the front door for one-call orientation around a code location
// (MCP: locate). It mirrors the MCP tool: give a path:line, a symbol, or a
// stack frame and it resolves the enclosing symbol plus callers, tests, nearest
// doc, last commit, and related notes.
func cmdLocate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("locate", flag.ContinueOnError)
	setHelp(fs,
		"One-call orientation around a code location (MCP: locate). Resolves a path:line / symbol / frame to its enclosing symbol + callers, tests, nearest doc, last commit, notes.",
		"dex locate [flags] [<path>] <symbol-or-path:line>",
		"dex locate '(*Server).RunStdio'",
		"dex locate internal/mcp/server.go:730",
		"dex locate --frame 'panic at internal/foo.go:42' --issues")
	frame := fs.String("frame", "", "a raw stack-trace frame line (file:line or symbol parsed out of it)")
	issues := fs.Bool("issues", false, "also list matching open GitHub issues via the gh CLI (best-effort)")
	k := fs.Int("k", 0, "max callers and notes to return (default 8, max 30)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())

	in := mcp.LocateInput{Issues: *issues, K: *k}
	switch {
	case *frame != "":
		in.Frame = *frame
		if len(rest) != 0 {
			return fmt.Errorf("locate: pass either --frame or a positional target, not both")
		}
	case len(rest) == 1:
		if reLocateRef.MatchString(rest[0]) {
			in.Ref = rest[0]
		} else {
			in.Symbol = rest[0]
		}
	case len(rest) == 0:
		return fmt.Errorf("locate needs a <symbol-or-path:line> (or --frame); path defaults to cwd")
	default:
		return fmt.Errorf("locate takes one target (got %d); quote symbols/refs that contain spaces", len(rest))
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	in.ProjectRoot = p.Root
	s, _ := newServerFromEnv(base)
	out, err := s.Locate(ctx, in)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return nil
	}
	renderLocateText(out)
	return nil
}

// renderLocateText prints the human view of a locate bundle.
func renderLocateText(out mcp.LocateOutput) {
	loc := out.Path
	if out.StartLine > 0 {
		loc = fmt.Sprintf("%s:%d-%d", out.Path, out.StartLine, out.EndLine)
	}
	fmt.Printf("%s (%s)  %s", out.Symbol, out.Kind, loc)
	if out.Risk != "" {
		fmt.Printf("   [risk: %s]", out.Risk)
	}
	fmt.Println()
	if len(out.Callers) > 0 {
		fmt.Printf("  callers (%d):\n", len(out.Callers))
		for _, c := range out.Callers {
			cloc := c.Path
			if c.CallSiteLine > 0 {
				cloc = fmt.Sprintf("%s:%d", c.CallSitePath, c.CallSiteLine)
			}
			fmt.Printf("    %s  (%s)  %s\n", c.QualifiedName, c.Kind, cloc)
		}
	}
	if len(out.Tests) > 0 {
		fmt.Printf("  tests: %v\n", out.Tests)
	}
	if out.NearestDoc != "" {
		fmt.Printf("  doc: %s\n", out.NearestDoc)
	}
	if out.LastCommit != "" {
		fmt.Printf("  last: %s  (%s)\n", out.LastCommit, out.LastAuthor)
	}
	if len(out.Issues) > 0 {
		fmt.Printf("  issues:\n")
		for _, is := range out.Issues {
			fmt.Printf("    %s\n", is)
		}
	}
	if out.Hint != "" {
		fmt.Printf("  hint: %s\n", out.Hint)
	}
}
