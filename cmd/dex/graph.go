package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// cmdGraph dispatches `dex graph <subcommand>`. The leaf
// subcommands (neighbors / deps / callers / callees / export) mirror
// the MCP `graph_*` tools 1:1 so CLI and MCP feel like the same tool.
func cmdGraph(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("graph needs a subcommand: neighbors | deps | packages | callers | callees | links | backlinks | tags | cycles | path | diff | communities | export")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "index":
		return fmt.Errorf("`graph index` has been folded into `index` — use `dex index --graph=only <path>` (or just `dex index <path>`, which runs both phases)")
	case "neighbors":
		return cmdGraphNeighbors(ctx, rest)
	case "deps":
		return cmdGraphDeps(ctx, rest)
	case "packages":
		return cmdGraphPackages(ctx, rest)
	case "callers":
		return cmdGraphCallers(ctx, rest)
	case "callees":
		return cmdGraphCallees(ctx, rest)
	case "links":
		return cmdGraphLinks(ctx, rest)
	case "backlinks":
		return cmdGraphBacklinks(ctx, rest)
	case "tags":
		return cmdGraphTags(ctx, rest)
	case "cycles":
		return cmdGraphCycles(ctx, rest)
	case "path":
		return cmdGraphPath(ctx, rest)
	case "diff":
		return cmdGraphDiff(ctx, rest)
	case "communities":
		return cmdGraphCommunities(ctx, rest)
	case "export":
		return cmdGraphExport(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex graph neighbors   [<path>] <file> <line>  vector neighbours of a chunk (MCP: graph_neighbors)
  dex graph deps        [<path>] [flags]        imports edges (MCP: deps)
                                                    --file=<rel>  --package=<full>
  dex graph packages    [<path>]                whole internal package import DAG
  dex graph callers     [<path>] <name>         incoming calls edges (MCP: callers)
                                                    --package=<pkg>  --k=<n>
  dex graph callees     [<path>] <name>         outgoing calls edges (MCP: callees)
                                                    --package=<pkg>  --k=<n>
  dex graph links       [<path>] <doc>          docs this doc links to (MCP: graph_links)
                                                    --k=<n>
  dex graph backlinks   [<path>] <doc>          docs that link to this doc (MCP: graph_backlinks)
                                                    --k=<n>
  dex graph tags        [<path>] [--tag=<t>|--doc=<d>]
                                                    tag→docs or doc→tags (MCP: graph_tags)
                                                    --k=<n>
  dex graph cycles      [<path>]                call-graph SCCs ≥ size 2 (MCP: graph_cycles)
                                                    --min-size=<n>  --k=<n>
  dex graph path        [<path>] <src> <dst>    shortest call/import path (MCP: path)
                                                    --package=<pkg>  --max-depth=<n>
  dex graph diff        [<path>]                blast-radius of current git diff (MCP: diff)
                                                    --ref=<ref>  --depth=<n>
  dex graph communities [<path>]                Louvain communities (MCP: clusters)
                                                    --min-members=<n>  --k=<n>  --top-k=<n>
  dex graph export      [<path>] [--output=<dir>]
                                                    dump nodes/edges as JSONL
  (path defaults to cwd when omitted)

note:
  'graph index' is gone — use 'dex index --graph=only <path>'.
  Plain 'dex index <path>' runs both chunk and graph phases.`)
		return nil
	default:
		return fmt.Errorf("unknown graph subcommand: %s (have: neighbors, deps, packages, callers, callees, links, backlinks, tags, cycles, path, diff, communities, export)", sub)
	}
}

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

func cmdGraphDeps(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph deps", flag.ContinueOnError)
	setHelp(fs,
		"Return `imports` edges for a file or package (MCP: deps).",
		"dex graph deps [flags] [<path>]")
	file := fs.String("file", "", "relative file path inside the project (resolved to its package)")
	pkg := fs.String("package", "", "full package path (e.g. 'github.com/foo/bar/internal/baz'); takes precedence over --file")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph deps takes no extra positional args (got %v)", rest)
	}
	if *file == "" && *pkg == "" {
		return fmt.Errorf("graph deps needs --file=<rel> or --package=<full>")
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
	out, err := s.GraphDeps(ctx, mcp.GraphDepsInput{
		Path:        *file,
		Package:     *pkg,
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
	fmt.Printf("package: %s\n", out.Package)
	if len(out.Imports) == 0 {
		fmt.Println("(no import edges)")
		return nil
	}
	for _, dep := range out.Imports {
		fmt.Printf("  → %s\n", dep.ToPackage)
	}
	return nil
}

func cmdGraphPackages(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph packages", flag.ContinueOnError)
	setHelp(fs,
		"Return the whole internal package import DAG with per-package in/out-degree + PageRank (MCP: graph_packages).",
		"dex graph packages [flags] [<path>]")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph packages takes no extra positional args (got %v)", rest)
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
	out, err := s.PackageGraph(ctx, mcp.PackageGraphInput{ProjectRoot: p.Root})
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

func cmdGraphCallers(ctx context.Context, args []string) error {
	return runGraphCallEdges(ctx, args, true)
}

func cmdGraphCallees(ctx context.Context, args []string) error {
	return runGraphCallEdges(ctx, args, false)
}

func runGraphCallEdges(ctx context.Context, args []string, callers bool) error {
	name := "graph callees"
	rel := "callees"
	helpOneLiner := "Outgoing `calls` edges (MCP: callees). Go-only today."
	if callers {
		name = "graph callers"
		rel = "callers"
		helpOneLiner = "Incoming `calls` edges (MCP: callers). Go-only today."
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	setHelp(fs, helpOneLiner, "dex "+name+" [flags] [<path>] <name>")
	k := fs.Int("k", 12, "max hits to return (default 12, max 50)")
	pkg := fs.String("package", "", "package path filter (when the same name is defined in multiple packages)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("%s needs a <name> (path defaults to cwd)", name)
		}
		return fmt.Errorf("%s takes one <name> (got %d extra args)", name, len(rest)-1)
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	in := mcp.CallEdgeInput{
		Name:        rest[0],
		Package:     *pkg,
		ProjectRoot: p.Root,
		K:           *k,
	}
	s, _ := newServerFromEnv(base)
	var out mcp.CallEdgeOutput
	if callers {
		out, err = s.GraphCallers(ctx, in)
	} else {
		out, err = s.GraphCallees(ctx, in)
	}
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
	if len(out.Targets) == 0 {
		fmt.Fprintln(os.Stderr, "no targets matched")
		return nil
	}
	fmt.Printf("targets (%d):\n", len(out.Targets))
	for _, t := range out.Targets {
		fmt.Printf("  %s  (%s)  %s\n", t.QualifiedName, t.Kind, t.Package)
	}
	fmt.Println()
	if len(out.Hits) == 0 {
		fmt.Printf("no %s\n", rel)
		return nil
	}
	fmt.Printf("%s (%d):\n", rel, len(out.Hits))
	for i, h := range out.Hits {
		loc := fmt.Sprintf("%s:%d", h.Path, h.StartLine)
		header := fmt.Sprintf("─── #%d %s  (%s)", i+1, h.QualifiedName, h.Kind)
		fmt.Println(header)
		fmt.Printf("  def: %s\n", loc)
		if h.CallSitePath != "" {
			fmt.Printf("  call site: %s:%d\n", h.CallSitePath, h.CallSiteLine)
		}
		if h.Role != "" {
			fmt.Printf("  role: %s\n", h.Role)
		}
		if h.Content != "" {
			for line := range strings.SplitSeq(strings.TrimRight(h.Content, "\n"), "\n") {
				fmt.Printf("  │ %s\n", line)
			}
			if h.Truncated {
				fmt.Println("  │ … (truncated; Read the file for the rest)")
			}
		}
		fmt.Println()
	}
	return nil
}

func cmdGraphLinks(ctx context.Context, args []string) error {
	return runGraphDocEdges(ctx, args, false)
}

func cmdGraphBacklinks(ctx context.Context, args []string) error {
	return runGraphDocEdges(ctx, args, true)
}

// runGraphDocEdges mirrors the MCP graph_links / graph_backlinks tools:
// markdown doc-graph traversal over `links`/`wikilinks` edges. backlinks
// walks incoming edges ("what links here"), otherwise outgoing.
func runGraphDocEdges(ctx context.Context, args []string, backlinks bool) error {
	name := "graph links"
	rel := "links"
	helpOneLiner := "Outgoing doc `links`/`wikilinks` (MCP: graph_links)."
	if backlinks {
		name = "graph backlinks"
		rel = "backlinks"
		helpOneLiner = "Incoming doc `links`/`wikilinks` (MCP: graph_backlinks)."
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	setHelp(fs, helpOneLiner, "dex "+name+" [flags] [<path>] <doc>")
	k := fs.Int("k", 50, "max hits to return (default 50, max 200)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("%s needs a <doc> path (path defaults to cwd)", name)
		}
		return fmt.Errorf("%s takes one <doc> (got %d extra args)", name, len(rest)-1)
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	in := mcp.DocLinkInput{
		Doc:         rest[0],
		ProjectRoot: p.Root,
		K:           *k,
	}
	s, _ := newServerFromEnv(base)
	var out mcp.DocLinkOutput
	if backlinks {
		out, err = s.GraphBacklinks(ctx, in)
	} else {
		out, err = s.GraphLinks(ctx, in)
	}
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
	if len(out.Targets) == 0 {
		fmt.Fprintln(os.Stderr, "no document matched")
		return nil
	}
	fmt.Printf("doc: %s\n\n", out.Targets[0].Doc)
	if len(out.Hits) == 0 {
		fmt.Printf("no %s\n", rel)
		return nil
	}
	fmt.Printf("%s (%d):\n", rel, len(out.Hits))
	for i, h := range out.Hits {
		// TargetAnchor is the section the underlying link points at. For
		// outgoing links that section lives in the peer doc (h.Doc), so
		// render it as `doc#anchor`. For backlinks it's a section of the
		// *queried* doc, so show it separately as "→ #anchor".
		target := h.Doc
		suffix := ""
		if h.TargetAnchor != "" {
			if backlinks {
				suffix = "  → #" + h.TargetAnchor
			} else {
				target += "#" + h.TargetAnchor
			}
		}
		fmt.Printf("─── #%d %s  (%s)%s\n", i+1, target, h.Kind, suffix)
		if h.LinkSitePath != "" {
			fmt.Printf("  link site: %s:%d\n", h.LinkSitePath, h.LinkSiteLine)
		}
	}
	return nil
}

// cmdGraphTags mirrors the MCP graph_tags tool: --tag=<t> lists the
// documents carrying a tag (ranked by importance); --doc=<d> lists the
// tags a document carries.
func cmdGraphTags(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph tags", flag.ContinueOnError)
	setHelp(fs, "Query the markdown tag graph (MCP: graph_tags).", "dex graph tags [flags] [<path>]")
	tag := fs.String("tag", "", "a #tag (without #) — list documents carrying it")
	docFlag := fs.String("doc", "", "a document path — list the tags it carries")
	k := fs.Int("k", 100, "max items to return (default 100, max 500)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph tags takes no positional args besides an optional path; use --tag or --doc")
	}
	if *tag == "" && *docFlag == "" {
		return fmt.Errorf("graph tags needs --tag=<t> or --doc=<d>")
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
	out, err := s.GraphTags(ctx, mcp.TagInput{Tag: *tag, Doc: *docFlag, ProjectRoot: p.Root, K: *k})
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
	fmt.Printf("%s of %q (%d):\n", out.Result, out.Query, len(out.Items))
	for _, it := range out.Items {
		fmt.Printf("  %s\n", it)
	}
	return nil
}

// parsePositiveInt is a tiny CLI helper for arg-parsing positional
// integers (e.g. `<line>`). Returns an error with the flag/arg name so
// the user knows which token failed.
func parsePositiveInt(name, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer (got %q)", name, raw)
	}
	return v, nil
}

// graphIndexResult is the JSON payload emitted by `index --graph=only --format=json`.
type graphIndexResult struct {
	Project    string   `json:"project"`
	Packages   int      `json:"packages"`
	Nodes      int64    `json:"nodes"`
	Edges      int64    `json:"edges"`
	Pruned     int64    `json:"pruned_nodes"`
	PrunedEdge int64    `json:"pruned_edges"`
	Linked     int      `json:"linked_to_chunks"`
	ElapsedMS  int64    `json:"elapsed_ms"`
	Warnings   []string `json:"warnings,omitempty"`
}

// runGraphPhase extracts the Go static graph for p and upserts into st.
// Shared by `index` (Phase 2) and `index --graph=only`.
func runGraphPhase(ctx context.Context, p *proj.Project, st *store.Store, verbose bool) (*graph.Stats, error) {
	gx := graph.New(p, graph.NewStoreAdapter(st), graph.Options{
		Verbose: verbose,
		Logger:  cliLogger(),
	})
	return gx.Run(ctx)
}

// reportGraphStats prints either a text summary or a JSON blob matching
// the old `graph index --format=json` schema, so existing scripts can
// migrate to `index --graph=only --format=json` without a payload change.
func reportGraphStats(project string, stats *graph.Stats, format string) error {
	switch format {
	case "json":
		out := graphIndexResult{
			Project:    project,
			Packages:   stats.Packages,
			Nodes:      stats.NodesUpserted,
			Edges:      stats.EdgesUpserted,
			Pruned:     stats.NodesPruned,
			PrunedEdge: stats.EdgesPruned,
			Linked:     stats.LinkedToChunks,
			ElapsedMS:  stats.Elapsed.Milliseconds(),
			Warnings:   stats.Warnings,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	default:
		fmt.Fprintf(os.Stderr, "  graph: %d packages  %d nodes  %d edges  %d linked  pruned %d/%d  in %s\n",
			stats.Packages, stats.NodesUpserted, stats.EdgesUpserted,
			stats.LinkedToChunks, stats.NodesPruned, stats.EdgesPruned, stats.Elapsed)
		if len(stats.Warnings) > 0 {
			fmt.Fprintf(os.Stderr, "  warnings: %d\n", len(stats.Warnings))
			for _, w := range stats.Warnings {
				fmt.Fprintf(os.Stderr, "    %s\n", w)
			}
		}
		return nil
	}
}

// embedGraphNodes embeds all graph_nodes whose vec_hash differs from
// content_hash (un-embedded or stale). Returns the number of nodes embedded.
func embedGraphNodes(ctx context.Context, st *store.Store, em embed.Embedder, verbose bool) (int, error) {
	const batchSize = 256
	total := 0
	for {
		rows, err := st.NodesNeedingEmbed(ctx, batchSize)
		if err != nil {
			return total, fmt.Errorf("embedGraphNodes: fetch: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		texts := make([]string, len(rows))
		for i, r := range rows {
			texts[i] = r.EmbedText
		}
		vecs, err := em.Embed(ctx, texts)
		if err != nil {
			return total, fmt.Errorf("embedGraphNodes: embed: %w", err)
		}
		if err := st.SetNodeVecs(ctx, rows, vecs); err != nil {
			return total, fmt.Errorf("embedGraphNodes: set: %w", err)
		}
		total += len(rows)
		if verbose {
			fmt.Printf("  [graph-embed] embedded %d nodes (total %d)\n", len(rows), total)
		}
	}
	return total, nil
}

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
	defer st.Close()

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
		"Find strongly connected components (call cycles / mutual recursion) in the call graph (MCP: graph_cycles).",
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

func cmdGraphPath(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph path", flag.ContinueOnError)
	setHelp(fs,
		"Find the shortest call/import path between two symbols (MCP: path).",
		"dex graph path [flags] [<path>] <src> <dst>")
	pkg := fs.String("package", "", "package path filter for both src and dst")
	maxDepth := fs.Int("max-depth", 8, "BFS depth limit (default 8, max 15)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 2 {
		return fmt.Errorf("graph path needs <src> <dst> (got %d positional arg(s))", len(rest))
	}
	src, dst := rest[0], rest[1]
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.GraphPath(ctx, mcp.PathInput{
		Src:         src,
		Dst:         dst,
		Package:     *pkg,
		MaxDepth:    *maxDepth,
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
	fmt.Printf("path from %q to %q (%d hops):\n\n", out.Src, out.Dst, len(out.Path))
	for i, hop := range out.Path {
		loc := hop.Path
		if hop.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", hop.Path, hop.StartLine)
		}
		if i == 0 {
			fmt.Printf("  [src] %s  (%s)  %s\n", hop.QualifiedName, hop.Kind, loc)
		} else {
			fmt.Printf("   ─%s─▶ %s  (%s)  %s\n", hop.EdgeKind, hop.QualifiedName, hop.Kind, loc)
		}
	}
	return nil
}

func cmdGraphDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph diff", flag.ContinueOnError)
	setHelp(fs,
		"Blast-radius of the current git diff: changed symbols and their transitive callers (MCP: diff).",
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

func cmdGraphCommunities(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph communities", flag.ContinueOnError)
	setHelp(fs,
		"List Louvain communities in the call/import graph (MCP: clusters).",
		"dex graph communities [flags] [<path>]")
	minMembers := fs.Int("min-members", 3, "min community size (default 3)")
	k := fs.Int("k", 20, "max communities to return (default 20)")
	topK := fs.Int("top-k", 10, "max members per community (default 10)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph communities takes no extra positional args (got %v)", rest)
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
	fmt.Printf("%d communities (total=%d", len(out.Communities), out.Total)
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
