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

// cmdRefs is the front door for type-precise symbol queries (MCP: refs):
// references, implementations, supertypes, subtypes — all via go/types, no index.
func cmdRefs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("refs", flag.ContinueOnError)
	setHelp(fs,
		"Type-precise Go symbol queries via go/types (MCP: refs): references, implementations, supertypes, subtypes.",
		"dex refs [flags] [<path>] <action> <symbol>",
		"dex refs references mcp.NewServer",
		"dex refs implementations toolSurface",
		"dex refs supertypes (*Server).Run",
		"dex refs subtypes Animal --format json")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}

	path, rest := splitProjectArg(fs.Args())
	if len(rest) < 2 {
		return fmt.Errorf("refs needs <action> and <symbol> (got %d arg(s)); path defaults to cwd\n"+
			"  actions: references, implementations, supertypes, subtypes", len(rest))
	}
	action, symbol := rest[0], strings.Join(rest[1:], " ")

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.Refs(ctx, mcp.RefsInput{Action: action, Symbol: symbol, ProjectRoot: p.Root})
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
	if len(out.Sites) == 0 {
		fmt.Printf("refs %s %s — no results\n", out.Action, out.Symbol)
		return nil
	}
	fmt.Printf("refs %s %s — %d result(s):\n", out.Action, out.Symbol, len(out.Sites))
	for _, s := range out.Sites {
		fmt.Printf("  %s:%d  [%s]\n", s.Path, s.Line, s.Kind)
	}
	return nil
}
