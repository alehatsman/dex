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

// cmdCheck verifies a batch of file:line[:symbol] references against the index
// (MCP: check). Useful after code changes to confirm cited locations are valid.
func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	setHelp(fs,
		"Verify file:line[:symbol] references against the index (MCP: check).",
		"dex check [flags] [<path>] <ref...>",
		"dex check internal/mcp/server.go:47:check",
		"dex check internal/store/store_search.go:1029",
		"dex check --format json internal/mcp/server.go:47 internal/mcp/server.go:100:nonexistent")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) == 0 {
		return fmt.Errorf("check needs at least one <ref> (file:line or file:line:symbol)")
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

	claims := make([]mcp.ClaimRef, len(rest))
	for i, r := range rest {
		claims[i] = mcp.ClaimRef{Ref: r}
	}
	out, err := s.Check(ctx, mcp.CheckInput{
		ProjectRoot: p.Root,
		Claims:      claims,
	})
	if err != nil {
		return err
	}
	// Compute failure once, independent of render format, so JSON mode
	// (the mode scripts/CI use) signals failure via exit code too.
	anyBad := false
	for _, r := range out.Results {
		if checkStatusFailed(r.Status) {
			anyBad = true
			break
		}
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		if anyBad {
			os.Exit(1)
		}
		return nil
	}

	for _, r := range out.Results {
		switch r.Status {
		case "ok":
			sym := r.SymbolAt
			if sym == "" {
				sym = "(file indexed)"
			}
			fmt.Printf("ok      %s  — %s\n", r.Ref, sym)
		case "moved":
			fmt.Printf("moved   %s  → %s\n", r.Ref, r.FoundAt)
		case "gone":
			fmt.Printf("gone    %s\n", r.Ref)
		case "no_file":
			fmt.Printf("no_file %s\n", r.Ref)
		case "parse_error":
			fmt.Printf("parse?  %s\n", r.Ref)
		default:
			fmt.Printf("%-8s %s\n", strings.TrimRight(r.Status, " "), r.Ref)
		}
	}
	if anyBad {
		os.Exit(1)
	}
	return nil
}

// checkStatusFailed reports whether a check result status counts as a
// verification failure. It is the single source of truth for the non-zero
// exit code, consulted before the text/JSON render split so both modes agree.
func checkStatusFailed(status string) bool {
	switch status {
	case "moved", "gone", "no_file", "parse_error":
		return true
	}
	return false
}
