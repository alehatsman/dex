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

// cmdRehearse type-checks a hypothetical edit in-memory and reports new type
// errors + broken files + tests to run, without writing anything (MCP: rehearse_patch).
func cmdRehearse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rehearse_patch", flag.ContinueOnError)
	setHelp(fs,
		"Type-check a hypothetical edit in-memory and return new type errors, broken files,\n"+
			"and tests to run — without writing anything (MCP: rehearse_patch). Go-only in v1.",
		"dex rehearse_patch [flags] [<path>] --edits <json> | --file <path> --contents <str>",
		"dex rehearse_patch --edits '[{\"path\":\"pkg/foo.go\",\"start_byte\":42,\"end_byte\":55,\"replacement\":\"NewName\"}]'",
		"dex rehearse_patch --file internal/store/store.go --contents \"$(cat /tmp/store_hypo.go)\"")
	editsJSON := fs.String("edits", "", "JSON array of {path,start_byte,end_byte,replacement} splices")
	filePath := fs.String("file", "", "project-relative path of a whole-file replacement (pair with --contents)")
	fileContents := fs.String("contents", "", "new file contents for --file")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("rehearse_patch takes no positional args besides [<path>] (got %d extra)", len(rest))
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return err
	}

	in := mcp.RehearseInput{ProjectRoot: p.Root}

	if *editsJSON != "" {
		var raw []mcp.RehearseEdit
		if err := json.Unmarshal([]byte(*editsJSON), &raw); err != nil {
			return fmt.Errorf("--edits: %w", err)
		}
		in.Edits = raw
	}

	if *filePath != "" {
		in.Files = append(in.Files, mcp.RehearseFile{Path: *filePath, Contents: *fileContents})
	}

	s, _ := newServerFromEnv(base)
	out, err := s.Rehearse(ctx, in)
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
	if out.Compiles {
		fmt.Printf("COMPILES (overlay_etag=%s)\n", out.OverlayEtag)
	} else {
		fmt.Printf("DOES NOT COMPILE (overlay_etag=%s) — %d new error(s)\n", out.OverlayEtag, len(out.Diagnostics))
		for _, d := range out.Diagnostics {
			fmt.Printf("  %s:%d:%d: %s\n", d.Path, d.Line, d.Col, d.Message)
		}
	}
	if len(out.TestsToRun) > 0 {
		fmt.Printf("tests to run: %v\n", out.TestsToRun)
	}
	return nil
}
