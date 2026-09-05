package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

func cmdGraphExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph export", flag.ContinueOnError)
	setHelp(fs,
		"Dump graph_nodes/graph_edges as JSONL.",
		"dex graph export [--output=<dir>] [<path>]")
	output := fs.String("output", "", "output directory (default: <project>/.dex/graph)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph export takes no extra positional args (got %v)", rest)
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no index at %s — run `dex index %s` first", p.DBPath, p.Root)
		}
		return err
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	outDir := *output
	if outDir == "" {
		outDir = filepath.Join(p.Root, ".dex", "graph")
	}
	if err := graph.ExportJSONL(ctx, graph.NewStoreAdapter(st), outDir); err != nil {
		return err
	}
	fmt.Printf("✓ graph exported to %s\n", outDir)
	fmt.Printf("  nodes: %s\n", filepath.Join(outDir, "nodes.jsonl"))
	fmt.Printf("  edges: %s\n", filepath.Join(outDir, "edges.jsonl"))
	return nil
}

func cmdGraphCycles(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph cycles", flag.ContinueOnError)
	setHelp(fs,
		"Find strongly connected components (call cycles / mutual recursion) in the call graph (CLI-only).",
		"dex graph cycles [flags] [<path>]")
	minSize := fs.Int("min-size", 2, "minimum SCC size to include (default 2)")
	k := fs.Int("k", 20, "max cycles to return (default 20, max 100)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph cycles takes no extra positional args (got %v)", rest)
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
	out, err := s.GraphCycles(ctx, mcp.CyclesInput{
		ProjectRoot: p.Root,
		MinSize:     *minSize,
		K:           *k,
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
	if len(out.Cycles) == 0 {
		fmt.Printf("no call cycles found (total=%d)\n", out.Total)
		return nil
	}
	fmt.Printf("%d call cycles (total=%d):\n\n", len(out.Cycles), out.Total)
	for i, c := range out.Cycles {
		fmt.Printf("─── cycle #%d  (size %d)\n", i+1, c.Size)
		for _, n := range c.Nodes {
			loc := n.Path
			if n.StartLine > 0 {
				loc = fmt.Sprintf("%s:%d", n.Path, n.StartLine)
			}
			fmt.Printf("  %s  (%s)  %s\n", n.QualifiedName, n.Kind, loc)
		}
		fmt.Println()
	}
	return nil
}

// cmdGraphPath was folded into `dex query --kind=path --to <dst>` by #849
// (CLI collapse) — the shortest call/import path between two symbols is
// served by internal/mcp's trace lane directly, no CLI-local reimplementation
// needed anymore.

func cmdGraphDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph diff", flag.ContinueOnError)
	setHelp(fs,
		"Blast-radius of the current git diff: changed symbols and their transitive callers (CLI-only; "+
			"MCP covers this via review_diff / trace --dir impact, DEX_EXPERT).",
		"dex graph diff [flags] [<path>]")
	ref := fs.String("ref", "HEAD~1", "git ref to diff against (default HEAD~1)")
	depth := fs.Int("depth", 2, "BFS depth for caller traversal (default 2, max 5)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph diff takes no extra positional args (got %v)", rest)
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
	out, err := s.GraphDiff(ctx, mcp.DiffInput{
		Ref:         *ref,
		MaxDepth:    *depth,
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
	fmt.Printf("blast-radius vs %s (%d changed files, depth %d):\n", out.Ref, len(out.ChangedFiles), out.MaxDepth)
	fmt.Printf("changed: %s\n\n", strings.Join(out.ChangedFiles, ", "))
	if len(out.Nodes) == 0 {
		fmt.Println("(no transitive callers found)")
		return nil
	}
	fmt.Printf("%d impacted callers", out.Total)
	if out.Truncated {
		fmt.Printf(" (truncated to %d)", len(out.Nodes))
	}
	fmt.Println(":")
	prevDepth := -1
	for _, n := range out.Nodes {
		if n.Depth != prevDepth {
			fmt.Printf("\n  depth %d:\n", n.Depth)
			prevDepth = n.Depth
		}
		loc := n.Path
		if n.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", n.Path, n.StartLine)
		}
		fmt.Printf("    %s  (%s)  %s\n", n.QualifiedName, n.Kind, loc)
	}
	return nil
}
