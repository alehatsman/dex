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

func cmdGraphNeighbors(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph neighbors", flag.ContinueOnError)
	setHelp(fs,
		"Find chunks semantically related to a given chunk (MCP: graph_neighbors).",
		"dex graph neighbors [flags] [<path>] <file> <line>")
	k := fs.Int("k", 8, "number of related chunks to return (max 30)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 2 {
		if len(rest) < 2 {
			return fmt.Errorf("graph neighbors needs <file> <line> (path defaults to cwd)")
		}
		return fmt.Errorf("graph neighbors takes <file> <line> (got %d extra args)", len(rest)-2)
	}
	line, err := parsePositiveInt("line", rest[1])
	if err != nil {
		return err
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
	out, err := s.Related(ctx, mcp.RelatedInput{
		Path:        rest[0],
		StartLine:   line,
		ProjectRoot: p.Root,
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
	printSearchHitResult(out.Status, out.Hint, out.Project, out.Hits, 1500)
	return nil
}

func cmdGraphSimilar(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph similar", flag.ContinueOnError)
	setHelp(fs,
		"Find code blocks semantically near a given block (MCP: similar).",
		"dex graph similar [flags] [<path>] <file> <line>")
	k := fs.Int("k", 8, "number of similar blocks to return (max 30)")
	threshold := fs.Float64("threshold", 0, "drop hits below this cosine similarity (0..1; 0 keeps all)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 2 {
		if len(rest) < 2 {
			return fmt.Errorf("graph similar needs <file> <line> (path defaults to cwd)")
		}
		return fmt.Errorf("graph similar takes <file> <line> (got %d extra args)", len(rest)-2)
	}
	line, err := parsePositiveInt("line", rest[1])
	if err != nil {
		return err
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
	out, err := s.Related(ctx, mcp.RelatedInput{
		Path:        rest[0],
		StartLine:   line,
		ProjectRoot: p.Root,
		K:           *k,
		Threshold:   float32(*threshold),
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printSearchHitResult(out.Status, out.Hint, out.Project, out.Hits, 1500)
	return nil
}

func cmdGraphClones(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph clones", flag.ContinueOnError)
	setHelp(fs,
		"Find clusters of semantically near-duplicate code blocks (MCP: clones).",
		"dex graph clones [flags] [<path>]")
	pathPrefix := fs.String("path", "", "restrict the scan to blocks under this relative path prefix")
	threshold := fs.Float64("threshold", 0, "min cosine similarity for a duplicate edge (default 0.90)")
	minLines := fs.Int("min-lines", 0, "ignore blocks shorter than this many lines (default 6)")
	k := fs.Int("k", 0, "neighbours probed per block (default 10, max 50)")
	maxClusters := fs.Int("max-clusters", 0, "max clusters to return (default 20, max 100)")
	format := fs.String("format", "text", "output format: text | json | jsonl (gate findings)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph clones takes no extra positional args (got %v)", rest)
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
	out, err := s.Clones(ctx, mcp.ClonesInput{
		Path:        *pathPrefix,
		Threshold:   float32(*threshold),
		MinLines:    *minLines,
		K:           *k,
		MaxClusters: *maxClusters,
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
	if *format == "jsonl" {
		return writeFindingsJSONL(out.Status, out.GateFindings())
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	if len(out.Clusters) == 0 {
		fmt.Println("no near-duplicate blocks found")
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	for i, c := range out.Clusters {
		fmt.Printf("\n─── cluster %d  (%d blocks, ≥%.2f similar)\n", i+1, c.Size, c.Similarity)
		for _, m := range c.Members {
			name := m.Name
			if name == "" {
				name = m.Kind
			}
			fmt.Printf("  %s:%d-%d  %s\n", m.Path, m.StartLine, m.EndLine, name)
		}
	}
	return nil
}
