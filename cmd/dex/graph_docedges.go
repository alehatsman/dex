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
		if out.Hint != "" {
			fmt.Printf("hint: %s\n", out.Hint)
		}
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
