package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

// cmdOrient renders `dex orient` — the session-start orientation bundle (epic
// #316, story 6 / #348): the deterministic L0 overview plus an L1 zoom into the
// most-central cluster, so an agent (or a hook piping this into context) names
// the right package before any find(). Zero inference, byte-stable. It renders
// through codemap.RenderOrient — the same path `ask("")` uses — so the CLI and
// MCP orientation surfaces agree, and both agree with `dex map`.
func cmdOrient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orient", flag.ContinueOnError)
	setHelp(fs,
		"Session-start orientation: L0 repo overview + L1 zoom into the most-central cluster.",
		"dex orient [--l0 <tokens>] [--l1 <tokens>] [--format text|json] [<path>]")
	l0budget := fs.Int("l0", codemap.DefaultL0Budget, "token budget for the L0 overview")
	l1budget := fs.Int("l1", codemap.DefaultL1Budget, "token budget for the L1 zoom")
	minMembers := fs.Int("min-members", 3, "min cluster size to consider")
	k := fs.Int("k", 50, "max clusters to scan")
	topK := fs.Int("top-k", 25, "max symbols pulled per cluster")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("orient takes no extra positional args (got %v)", rest)
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
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return nil
	}

	clusters := adaptCommunities(out.Communities)
	bundle := codemap.RenderOrient(clusters, *l0budget, *l1budget)
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"zoom": "orient", "map": bundle})
	}
	fmt.Print(bundle)
	return nil
}
