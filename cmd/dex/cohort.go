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

// cmdCohort is the front door for interface cohesion (MCP: cohort): the set of
// types that must change in lockstep with an interface.
func cmdCohort(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cohort", flag.ContinueOnError)
	setHelp(fs,
		"Find the types that must change together with an interface (MCP: cohort): complete implementors + near-misses missing methods.",
		"dex cohort [flags] [<path>] <interface>",
		"dex cohort toolSurface",
		"dex cohort mcp.toolSurface --format json")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		return fmt.Errorf("cohort needs one <interface> name (got %d); path defaults to cwd", len(rest))
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
	out, err := s.Cohort(ctx, mcp.CohortInput{Interface: rest[0], ProjectRoot: p.Root})
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
	fmt.Printf("cohort of %s (%d methods) — %d complete, %d partial:\n",
		out.Interface, len(out.Methods), out.Complete, out.Partial)
	for _, m := range out.Members {
		loc := fmt.Sprintf("%s:%d", m.Path, m.Line)
		if m.Status == "complete" {
			fmt.Printf("  ✓ %s  %s\n", m.Type, loc)
		} else {
			fmt.Printf("  ✗ %s  %s  — missing: %v\n", m.Type, loc, m.Missing)
		}
	}
	if out.Hint != "" {
		fmt.Printf("hint: %s\n", out.Hint)
	}
	return nil
}
