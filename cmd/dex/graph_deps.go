package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

func cmdGraphDeps(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph deps", flag.ContinueOnError)
	setHelp(fs,
		"Return `imports` edges for a file or package (MCP: deps, DEX_EXPERT).",
		"dex graph deps [flags] [<project>] <path|package>")
	file := fs.String("file", "", "relative file path inside the project (resolved to its package)")
	pkg := fs.String("package", "", "full package path (e.g. 'github.com/foo/bar/internal/baz'); takes precedence over --file")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}

	// Accept a positional file-or-package target like the sibling graph verbs
	// (callers/callees/path), keeping --file/--package as explicit overrides
	// (#504). Grammar: `[<project>] <target>`; with an explicit flag set, only
	// an optional leading <project> path may precede it. splitProjectArg would
	// greedily eat a relative dir target as the project path, so positionals are
	// parsed by count here instead.
	var projArg, target string
	rest := fs.Args()
	switch {
	case *file != "" || *pkg != "":
		projArg, rest = splitProjectArg(rest)
		if len(rest) != 0 {
			return fmt.Errorf("graph deps: --file/--package already set, unexpected positional args %v", rest)
		}
	case len(rest) == 1:
		target = rest[0]
	case len(rest) == 2:
		projArg, target = rest[0], rest[1]
	case len(rest) == 0:
		return fmt.Errorf("graph deps needs a <path> positional (a file or package), or --file=<rel> / --package=<full>")
	default:
		return fmt.Errorf("graph deps takes [<project>] <path> (got %d positional args)", len(rest))
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(projArg, base)
	if err != nil {
		return err
	}

	inFile, inPkg := *file, *pkg
	if target != "" {
		inFile, inPkg = inferDepsTarget(p.Root, target)
	}
	s, _ := newServerFromEnv(base)
	out, err := s.GraphDeps(ctx, mcp.GraphDepsInput{
		Path:        inFile,
		Package:     inPkg,
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

// inferDepsTarget maps a positional `graph deps` target to either a relative
// file Path or a full import Package, mirroring how the server resolves each
// (Path → NodesByPath, Package → NodesByPackage). Resolution is filesystem-
// grounded relative to projRoot: an existing file is a Path; an existing
// directory is a package dir resolved to a representative .go file (the server
// maps a file → its package); anything that does not exist on disk is treated
// as a full import path (Package).
func inferDepsTarget(projRoot, target string) (file, pkg string) {
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projRoot, target)
	}
	info, err := os.Stat(abs)
	switch {
	case err == nil && info.IsDir():
		if f := firstGoFile(abs); f != "" {
			if rel, rerr := filepath.Rel(projRoot, f); rerr == nil {
				return rel, ""
			}
		}
		// Empty/relless dir — hand the dir through as Path so the server
		// reports a clean not-found rather than us guessing a package.
		return target, ""
	case err == nil:
		if rel, rerr := filepath.Rel(projRoot, abs); rerr == nil {
			return rel, ""
		}
		return target, ""
	default:
		// Not on disk → a fully-qualified import path.
		return "", target
	}
}

// firstGoFile returns a representative .go file in dir (non-test preferred), or
// "" if none. Used to resolve a package directory to a file the graph index
// keys by. os.ReadDir is name-sorted, so the choice is deterministic.
func firstGoFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var testFallback string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			if testFallback == "" {
				testFallback = filepath.Join(dir, e.Name())
			}
			continue
		}
		return filepath.Join(dir, e.Name())
	}
	return testFallback
}

func cmdGraphPackages(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("graph packages", flag.ContinueOnError)
	setHelp(fs,
		"Return the whole internal package import DAG with per-package in/out-degree + PageRank (CLI-only).",
		"dex graph packages [flags] [<path>]")
	format := fs.String("format", "text", "output format: text | json")
	level := fs.String("level", "module", "aggregation level: module | project (roll JS/TS modules up to workspace packages)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("graph packages takes no extra positional args (got %v)", rest)
	}
	if *level != "module" && *level != "project" {
		return fmt.Errorf("graph packages --level must be module or project (got %q)", *level)
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
	out, err := s.PackageGraph(ctx, mcp.PackageGraphInput{ProjectRoot: p.Root, Level: *level})
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
