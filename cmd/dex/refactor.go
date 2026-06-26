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

// cmdRefactor is the front door for type-precise edit planning (MCP: refactor).
// dex never writes files — it prints the edit triples for the caller to apply.
// v1 supports op=rename_symbol for Go.
func cmdRefactor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("refactor", flag.ContinueOnError)
	setHelp(fs,
		"Plan a type-precise rename and print byte-exact edit triples (MCP: refactor). dex never writes — you apply them.",
		"dex refactor [flags] [<path>] <symbol> <to>",
		"dex refactor Greet Welcome",
		"dex refactor '(*Server).Run' Start --format json")
	op := fs.String("op", "rename_symbol", "operation (v1: rename_symbol only)")
	etag := fs.String("etag", "", "plan etag from a prior call; reports 'stale' if files changed since")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 2 {
		return fmt.Errorf("refactor needs <symbol> <to> (got %d args); path defaults to cwd", len(rest))
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
	out, err := s.Refactor(ctx, mcp.RefactorInput{
		Op:          *op,
		Symbol:      rest[0],
		To:          rest[1],
		Etag:        *etag,
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
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return nil
	}
	renderRefactorText(out)
	return nil
}

func renderRefactorText(out mcp.RefactorOutput) {
	fmt.Printf("rename %s → %s  (%s) — %d edit(s) across %d file(s)",
		out.From, out.To, out.Object, len(out.Edits), out.Files)
	if out.Etag != "" {
		fmt.Printf("  [etag %s]", out.Etag)
	}
	fmt.Println()
	for _, w := range out.Warnings {
		fmt.Printf("  ⚠ %s\n", w)
	}
	var lastPath string
	for _, e := range out.Edits {
		if e.Path != lastPath {
			fmt.Printf("  %s:\n", e.Path)
			lastPath = e.Path
		}
		fmt.Printf("    L%d  bytes %d-%d → %s\n", e.Line, e.StartByte, e.EndByte, e.Replacement)
	}
	fmt.Println("apply highest-offset-first per file (dex does not write files).")
}
