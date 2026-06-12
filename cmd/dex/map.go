package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/alehatsman/dex/internal/codemap"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

// cmdMap renders `dex map` — a deterministic, zero-inference orientation map
// (epic #316, story 1). With no --cluster it prints L0 (top clusters by
// PageRank); with --cluster <id> it prints L1 (that cluster in detail). It
// composes the existing Louvain communities + PageRank already in the graph —
// no model is called.
func cmdMap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	setHelp(fs,
		"Deterministic repo orientation map: top clusters (L0) or one cluster in detail (L1).",
		"dex map [--cluster <id>] [--budget <tokens>] [flags] [<path>]")
	cluster := fs.Int("cluster", -1, "zoom into one cluster by id (L1); omit for the repo overview (L0)")
	budget := fs.Int("budget", 0, "token budget (default 150 for L0, 1000 for L1)")
	minMembers := fs.Int("min-members", 3, "min cluster size to consider")
	k := fs.Int("k", 50, "max clusters to scan")
	topK := fs.Int("top-k", 25, "max symbols pulled per cluster")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("map takes no extra positional args (got %v)", rest)
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

	if *cluster >= 0 {
		c, ok := findCluster(clusters, *cluster)
		if !ok {
			return fmt.Errorf("cluster #%d not found (try `dex map` to list clusters)", *cluster)
		}
		if *format == "json" {
			return printMapJSON("l1", []codemap.Cluster{c})
		}
		fmt.Print(codemap.RenderL1(c, *budget))
		return nil
	}

	if *format == "json" {
		return printMapJSON("l0", clusters)
	}
	fmt.Print(codemap.RenderL0(clusters, *budget))
	return nil
}

// adaptCommunities maps the MCP community projection into the renderer's input.
func adaptCommunities(comms []mcp.Community) []codemap.Cluster {
	clusters := make([]codemap.Cluster, 0, len(comms))
	for _, c := range comms {
		syms := make([]codemap.Symbol, 0, len(c.Members))
		for _, m := range c.Members {
			syms = append(syms, codemap.Symbol{
				QualifiedName: m.QualifiedName,
				Kind:          m.Kind,
				Pkg:           m.Package,
				Path:          m.Path,
				Line:          m.StartLine,
				PageRank:      m.PageRank,
			})
		}
		clusters = append(clusters, codemap.Cluster{ID: c.ID, Size: c.Size, Symbols: syms})
	}
	return clusters
}

func findCluster(clusters []codemap.Cluster, id int) (codemap.Cluster, bool) {
	for _, c := range clusters {
		if c.ID == id {
			return c, true
		}
	}
	return codemap.Cluster{}, false
}

// printMapJSON emits the adapted clusters as JSON, sorted by aggregate PageRank
// for L0 so machine consumers see the same order as the text view.
func printMapJSON(zoom string, clusters []codemap.Cluster) error {
	if zoom == "l0" {
		sort.SliceStable(clusters, func(i, j int) bool {
			return clusterWeight(clusters[i]) > clusterWeight(clusters[j])
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"zoom": zoom, "clusters": clusters})
}

func clusterWeight(c codemap.Cluster) float64 {
	var w float64
	for _, s := range c.Symbols {
		w += s.PageRank
	}
	return w
}
