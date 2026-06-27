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

// cmdMap renders `dex map` — a deterministic, zero-inference orientation map
// (epic #316, story 1). With no --cluster it prints the first-touch orientation
// bundle: the L0 overview (top clusters by PageRank) followed by an L1 zoom into
// the most-central cluster (#574 — the former `dex orient`). With --cluster <id>
// it zooms a chosen cluster instead. It composes the Louvain communities +
// PageRank already in the graph — no model is called.
func cmdMap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	setHelp(fs,
		"Deterministic repo orientation: the first-touch bundle (L0 overview + a zoom into the most-central cluster), one chosen cluster in detail (--cluster), or a task-focused region (--around / --around-diff).",
		"dex map [--cluster <id>] [--around <symbol>] [--around-diff <ref>] [--budget <tokens>] [flags] [<path>]")
	cluster := fs.Int("cluster", -1, "zoom into one cluster by id (L1); omit for the first-touch orientation bundle")
	around := fs.String("around", "", "render the region around a symbol — its callers ∪ callees — instead of the overview")
	aroundDiff := fs.String("around-diff", "", "render the blast radius of a git diff (the ref to diff against, e.g. HEAD~1)")
	budget := fs.Int("budget", 0, "token budget (default 150 for L0, 1000 for L1/region)")
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

	// #347 story 5: task-focused region. --around / --around-diff render the
	// call-graph neighborhood of a symbol or a diff's blast radius instead of
	// the Louvain overview; mutually exclusive with each other and --cluster.
	if *around != "" || *aroundDiff != "" {
		if *around != "" && *aroundDiff != "" {
			return fmt.Errorf("--around and --around-diff are mutually exclusive — pass one")
		}
		if *cluster >= 0 {
			return fmt.Errorf("--around cannot be combined with --cluster")
		}
		return cmdMapAround(ctx, s, p.Root, *around, *aroundDiff, *budget, *format)
	}

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
		return reportMapStatus(out.Status, out.Hint)
	}
	if len(out.Communities) == 0 {
		if out.Hint != "" {
			fmt.Fprintln(os.Stderr, out.Hint)
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
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"zoom": "l1", "clusters": []codemap.Cluster{c}})
		}
		fmt.Print(codemap.RenderL1(c, *budget))
		return nil
	}

	// Default (no --cluster): the first-touch orientation bundle — L0 overview
	// plus an auto-zoom into the most-central cluster. RenderOrient defaults the
	// budgets when zero (150 for L0, 1000 for the L1 zoom).
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"zoom": "orient", "map": codemap.RenderOrient(clusters,
			codemap.OrientExtras{Entrypoints: out.Entrypoints, ImportEdges: mcp.CodemapImportEdges(out.ImportEdges), Externals: out.Externals, Scale: mcp.CodemapScale(out.Scale)}, *budget, 0)})
	}
	fmt.Print(codemap.RenderOrient(clusters,
		codemap.OrientExtras{Entrypoints: out.Entrypoints, ImportEdges: mcp.CodemapImportEdges(out.ImportEdges), Externals: out.Externals, Scale: mcp.CodemapScale(out.Scale)}, *budget, 0))
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

// cmdMapAround renders a task-focused region (issue #347, story 5): the
// call-graph neighborhood of a symbol (--around) or the blast radius of a diff
// (--around-diff). It mirrors the MCP map verb, reusing the same exported
// region adapters so the two surfaces stay identical.
func cmdMapAround(ctx context.Context, s *mcp.Server, root, around, aroundDiff string, budget int, format string) error {
	var title string
	var syms []codemap.Symbol

	if aroundDiff != "" {
		d, err := s.GraphDiff(ctx, mcp.DiffInput{Ref: aroundDiff, ProjectRoot: root})
		if err != nil {
			return err
		}
		if d.Status != "ok" {
			return reportMapStatus(d.Status, d.Hint)
		}
		ref := d.Ref
		if ref == "" {
			ref = aroundDiff
		}
		title, syms = mcp.DiffTitle(ref), mcp.DiffSymbols(d)
	} else {
		cin := mcp.CallEdgeInput{Name: around, ProjectRoot: root}
		callers, err := s.GraphCallers(ctx, cin)
		if err != nil {
			return err
		}
		callees, err := s.GraphCallees(ctx, cin)
		if err != nil {
			return err
		}
		for _, st := range []mcp.CallEdgeOutput{callers, callees} {
			if st.Status != "ok" && st.Status != "not-found" {
				return reportMapStatus(st.Status, st.Hint)
			}
		}
		if callers.Status == "not-found" && callees.Status == "not-found" {
			return reportMapStatus("not-found", fmt.Sprintf("symbol %q not found in the call graph", around))
		}
		title, syms = mcp.AroundTitle(around), mcp.AroundSymbols(callers, callees)
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"zoom": "around", "title": title, "symbols": syms})
	}
	fmt.Print(codemap.RenderAround(title, syms, budget))
	return nil
}

// reportMapStatus prints a non-ok backend status (and hint) to stderr and
// returns nil — a missing index or empty graph is a user condition, not a CLI
// error. Shared by the L0/L1 and region paths.
func reportMapStatus(status, hint string) error {
	fmt.Fprintf(os.Stderr, "status: %s\n", status)
	if hint != "" {
		fmt.Fprintf(os.Stderr, "hint: %s\n", hint)
	}
	return nil
}
