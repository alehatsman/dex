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

func cmdGraphCallers(ctx context.Context, args []string) error {
	return runGraphCallEdges(ctx, args, true)
}

func cmdGraphCallees(ctx context.Context, args []string) error {
	return runGraphCallEdges(ctx, args, false)
}

func runGraphCallEdges(ctx context.Context, args []string, callers bool) error {
	name := "graph callees"
	rel := "callees"
	helpOneLiner := "Outgoing `calls` edges (folds into MCP trace --dir callees, DEX_EXPERT). Go is type-resolved; other langs name-based (tree-sitter)."
	if callers {
		name = "graph callers"
		rel = "callers"
		helpOneLiner = "Incoming `calls` edges (folds into MCP trace --dir callers, DEX_EXPERT). Go is type-resolved; other langs name-based (tree-sitter)."
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
	// Parity with the MCP trace verb: incoming unresolved imports are potential
	// hidden callers, so surface them for callers (not callees). #130.
	if callers {
		out.UnresolvedInbound = s.UnresolvedInboundForTargets(ctx, in.ProjectRoot, out.Targets)
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	// printInbound renders the unresolved-inbound block (#130) — shown in both the
	// no-callers and the has-callers text paths, since it matters most at zero.
	printInbound := func() {
		if len(out.UnresolvedInbound) == 0 {
			return
		}
		fmt.Printf("unresolved inbound (imports into this package dex could not bind to a symbol; name-based recall misses them):\n")
		for _, r := range out.UnresolvedInbound {
			fmt.Printf("  %s  ×%d\n", r.Specifier, r.Count)
		}
		fmt.Printf("hint: %s\n\n", mcp.UnresolvedInboundHint(out.UnresolvedInbound))
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
		if out.Hint != "" {
			fmt.Printf("hint: %s\n", out.Hint)
		}
		fmt.Println()
		printInbound()
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
	printInbound()
	return nil
}
