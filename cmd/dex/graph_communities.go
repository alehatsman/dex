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

func cmdGraphCommunities(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph clusters", flag.ContinueOnError)
	setHelp(fs,
		"List Louvain clusters in the call/import graph (MCP: clusters).",
		"dex graph clusters [flags] [<path>]")
	minMembers := fs.Int("min-members", 3, "min community size (default 3)")
	k := fs.Int("k", 20, "max clusters to return (default 20)")
	topK := fs.Int("top-k", 10, "max members per community (default 10)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph clusters takes no extra positional args (got %v)", rest)
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
	out, err := s.GraphCommunities(ctx, mcp.CommunitiesInput{
		MinMembers:  *minMembers,
		K:           *k,
		TopK:        *topK,
		ProjectRoot: p.Root,
	})
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
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	fmt.Printf("%d clusters (total=%d", len(out.Communities), out.Total)
	if out.Truncated {
		fmt.Printf(", truncated")
	}
	fmt.Println("):")
	for _, c := range out.Communities {
		fmt.Printf("\n─── community #%d  (size %d)\n", c.ID, c.Size)
		for _, m := range c.Members {
			loc := m.Path
			if m.StartLine > 0 {
				loc = fmt.Sprintf("%s:%d", m.Path, m.StartLine)
			}
			fmt.Printf("  %s  (%s)  %s\n", m.QualifiedName, m.Kind, loc)
		}
	}
	return nil
}
