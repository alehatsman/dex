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

// cmdGraphDeps was folded into `dex query --kind=deps` by #849 (CLI collapse,
// specs/query-unification.md) — see internal/mcp/query_misc.go for the ported
// inferDepsTarget/firstGoFile logic. `graph packages` below is unrelated and
// stays CLI-only (whole-repo package DAG report, no query equivalent).

func cmdGraphPackages(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph packages", flag.ContinueOnError)
	setHelp(fs,
		"Return the whole internal package import DAG with per-package in/out-degree + PageRank (CLI-only).",
		"dex graph packages [flags] [<path>]")
	format := fs.String("format", "text", "output format: text | json")
	level := fs.String("level", "module", "aggregation level: module | project (roll JS/TS modules up to workspace packages)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph packages takes no extra positional args (got %v)", rest)
	}
	if *level != "module" && *level != "project" {
		return fmt.Errorf("graph packages --level must be module or project (got %q)", *level)
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
	out, err := s.PackageGraph(ctx, mcp.PackageGraphInput{ProjectRoot: p.Root, Level: *level})
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
	fmt.Printf("%d packages, %d internal import edges\n", len(out.Nodes), len(out.Edges))
	for _, n := range out.Nodes {
		marker := ""
		if n.IsMain {
			marker = " [main]"
		}
		fmt.Printf("  in=%-3d out=%-3d pr=%.4f  %s%s\n", n.InDegree, n.OutDegree, n.PageRank, n.Package, marker)
	}
	return nil
}
