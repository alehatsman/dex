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

// writeFindingsJSONL prints findings as one JSON object per line (#155 P3 emit:
// `dex smells|clones --format jsonl`) — the shared gate finding schema the
// goq/findings aggregator ingests, making dex's own analysis gate-pluggable. A
// non-ok status (no-index / graph not built) yields no rows, not an error: this
// is a findings feed, consistent with the go-quality emitters.
func writeFindingsJSONL(status string, findings []mcp.GateFinding) error {
	if status != "ok" {
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	for _, f := range findings {
		if err := enc.Encode(f); err != nil {
			return err
		}
	}
	return nil
}

func cmdGraphSmells(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph smells", flag.ContinueOnError)
	setHelp(fs,
		"Surface code smells: long functions, dead exports, god files, god-nodes (MCP: smells).",
		"dex graph smells [flags] [<path>]")
	minFuncLines := fs.Int("min-func-lines", 0, "min function body length to flag as long (default 80)")
	minFileSymbols := fs.Int("min-file-symbols", 0, "min symbols per file to flag as a god file (default 30)")
	minGodCallers := fs.Int("min-god-node-callers", 0, "min in-degree to flag a god-node (default 20)")
	minGodPkgCallers := fs.Int("min-god-node-pkg-callers", 0, "min cross-pkg callers to flag a god-node (default 8)")
	limit := fs.Int("limit", 0, "max results per category (default 20)")
	format := fs.String("format", "text", "output format: text | json | jsonl (gate findings)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph smells takes no extra positional args (got %v)", rest)
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
	out, err := s.Smells(ctx, mcp.SmellsInput{
		MinFuncLines:         *minFuncLines,
		MinFileSymbols:       *minFileSymbols,
		MinGodNodeCallers:    *minGodCallers,
		MinGodNodePkgCallers: *minGodPkgCallers,
		Limit:                *limit,
		ProjectRoot:          p.Root,
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
	printSmellHits := func(title string, hits []SmellHitView) {
		if len(hits) == 0 {
			return
		}
		fmt.Printf("\n─── %s (%d)\n", title, len(hits))
		for _, h := range hits {
			loc := h.Path
			if h.StartLine > 0 {
				loc = fmt.Sprintf("%s:%d", h.Path, h.StartLine)
			}
			if h.Lines > 0 {
				fmt.Printf("  %s  (%s, %d lines)  %s\n", h.QualifiedName, h.Kind, h.Lines, loc)
			} else {
				fmt.Printf("  %s  (%s)  %s\n", h.QualifiedName, h.Kind, loc)
			}
		}
	}
	printSmellHits("long functions", toSmellHitViews(out.LongFunctions))
	printSmellHits("dead exports", toSmellHitViews(out.DeadExports))
	printSmellHits("god-nodes", toSmellHitViews(out.GodNodes))
	if len(out.GodFiles) > 0 {
		fmt.Printf("\n─── god files (%d)\n", len(out.GodFiles))
		for _, f := range out.GodFiles {
			fmt.Printf("  %s  (%d symbols)\n", f.Path, f.SymbolCount)
		}
	}
	return nil
}

// SmellHitView is the local shape shared by the smell categories so the text
// renderer can iterate them uniformly.
type SmellHitView struct {
	QualifiedName string
	Kind          string
	Path          string
	StartLine     int
	Lines         int
}

func toSmellHitViews(hits []mcp.SmellHit) []SmellHitView {
	out := make([]SmellHitView, len(hits))
	for i, h := range hits {
		out[i] = SmellHitView{
			QualifiedName: h.QualifiedName,
			Kind:          h.Kind,
			Path:          h.Path,
			StartLine:     h.StartLine,
			Lines:         h.Lines,
		}
	}
	return out
}

func cmdGraphRoutes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph routes", flag.ContinueOnError)
	setHelp(fs,
		"List detected HTTP/MCP/gRPC handlers and registration sites (MCP: routes).",
		"dex graph routes [flags] [<path>]")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph routes takes no extra positional args (got %v)", rest)
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
	out, err := s.Routes(ctx, mcp.RoutesInput{ProjectRoot: p.Root})
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
	fmt.Printf("%d routes:\n", out.Total)
	for _, r := range out.Routes {
		loc := r.Path
		if r.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", r.Path, r.StartLine)
		}
		line := fmt.Sprintf("  %s  (%s)  %s", r.QualifiedName, r.Kind, loc)
		if r.RegisteredBy != "" {
			line += fmt.Sprintf("  ← %s", r.RegisteredBy)
		}
		fmt.Println(line)
	}
	return nil
}
