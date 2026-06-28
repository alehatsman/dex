package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

func cmdBrief(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	setHelp(fs,
		"Build a task-specific context pack. Call before starting any coding task.",
		"dex brief [flags] [<path>] <task...>",
		"dex brief 'add rate limiting to the HTTP server'",
		"dex brief . 'fix the flaky test in store_test.go'")
	budget := fs.Int("budget", 6000, "approximate token budget")
	sections := fs.String("sections", "", "comma-separated sections: map,relevant_code,rules,tests,impact")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("brief needs a task description")
	}
	task := strings.Join(rest, " ")

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	in := mcp.BriefInput{
		Task:         task,
		BudgetTokens: *budget,
		ProjectRoot:  p.Root,
	}
	if *sections != "" {
		in.Sections = strings.Split(*sections, ",")
	}

	s, _ := newServerFromEnv(base)
	out, err := s.Brief(ctx, in)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	renderBriefText(out)
	return nil
}

func renderBriefText(out mcp.BriefOutput) {
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return
	}
	fmt.Printf("# Brief: %s\n\n", out.Task)
	if len(out.RelevantFiles) > 0 {
		fmt.Println("## Relevant files")
		for _, f := range out.RelevantFiles {
			fmt.Printf("  %s  (score=%.2f  mode=%s)\n", f.Path, f.Score, f.Mode)
		}
		fmt.Println()
	}
	if len(out.LocalRules) > 0 {
		fmt.Println("## Local rules")
		for _, r := range out.LocalRules {
			fmt.Printf("  %s\n", r)
		}
		fmt.Println()
	}
	if len(out.Tests) > 0 {
		fmt.Printf("## Tests to run: %v\n\n", out.Tests)
	}
	if len(out.Risks) > 0 {
		fmt.Println("## Risks")
		for _, r := range out.Risks {
			fmt.Printf("  - %s\n", r)
		}
		fmt.Println()
	}
	if len(out.NextCalls) > 0 {
		fmt.Println("## Next calls")
		for _, nc := range out.NextCalls {
			fmt.Printf("  %s(%v)  — %s\n", nc.Tool, nc.Args, nc.Reason)
		}
	}
}
