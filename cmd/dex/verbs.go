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

// CLI verb front doors mirroring the MCP tool facade (#345/#354/#427): the CLI
// verbs are the same names as the MCP tools — find / trace / map / read / ask —
// so an agent and a human share one vocabulary across the stdio MCP surface, the
// REST surface, and the CLI.

// cmdFind is the front door for semantic code search (MCP: find).
func cmdFind(ctx context.Context, args []string) error {
	return cmdSearchSemantic(ctx, args)
}

// splitTraceArgs peels trace's own flags (--direction/--dir/-d and --to) off the
// argv, returning the chosen direction and the remaining args to forward verbatim
// to the underlying graph subcommand (so its flags/rendering stay identical).
// help is true when a help flag was seen.
func splitTraceArgs(args []string) (dir string, fwd []string, help bool, err error) {
	dir = "callers"
	fwd = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--direction" || a == "--dir" || a == "-d":
			if i+1 >= len(args) {
				return "", nil, false, fmt.Errorf("trace: %s needs a value (callers|callees|path)", a)
			}
			dir, i = args[i+1], i+1
		case strings.HasPrefix(a, "--direction="):
			dir = strings.TrimPrefix(a, "--direction=")
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		case a == "--to":
			if i+1 >= len(args) {
				return "", nil, false, fmt.Errorf("trace: --to needs a destination symbol")
			}
			fwd = append(fwd, args[i+1]) // path's <dst> is positional downstream
			i++
		case strings.HasPrefix(a, "--to="):
			fwd = append(fwd, strings.TrimPrefix(a, "--to="))
		case a == "-h" || a == "--help" || a == "help":
			return dir, fwd, true, nil
		default:
			fwd = append(fwd, a)
		}
	}
	return dir, fwd, false, nil
}

// cmdTrace walks the static call graph from a symbol (MCP: trace). `--direction`
// selects the traversal and re-dispatches to the existing graph subcommand:
//
//	callers (default) -> dex graph callers
//	callees           -> dex graph callees
//	path              -> dex graph path (destination via --to or a 2nd arg)
//	impact            -> transitive caller blast-radius (risk tier + tests_to_run)
func cmdTrace(ctx context.Context, args []string) error {
	dir, fwd, help, err := splitTraceArgs(args)
	if err != nil {
		return err
	}
	if help {
		fmt.Fprintln(os.Stderr, `usage:
  dex trace [<path>] <name>                         incoming callers (default)
  dex trace [<path>] <name> --dir callees           outgoing callees
  dex trace [<path>] <src> --dir path --to <dst>    shortest call path
  dex trace [<path>] <name> --dir impact            transitive caller blast-radius

mirrors MCP `+"`trace`"+`; re-dispatches to `+"`dex graph callers|callees|path`"+` and impact.
flags after the name (-k, --package, --max-depth, --format) pass through.`)
		return nil
	}
	switch dir {
	case "callers":
		return cmdGraphCallers(ctx, fwd)
	case "callees":
		return cmdGraphCallees(ctx, fwd)
	case "path":
		return cmdGraphPath(ctx, fwd)
	case "impact":
		return cmdImpact(ctx, fwd)
	default:
		return fmt.Errorf("trace: --direction must be callers, callees, path, or impact (got %q)", dir)
	}
}

// cmdImpact reports the transitive blast-radius of a symbol — every function
// reachable by following `calls` edges in the callers direction, tagged with
// hop depth. Reached via `dex trace --dir impact` (MCP: trace --dir impact);
// it is no longer a standalone verb (#684).
func cmdImpact(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	setHelp(fs,
		"Transitive blast-radius via callers (MCP: trace --dir impact). Go is type-resolved; other langs name-based (tree-sitter, incomplete recall).",
		"dex trace [flags] [<path>] <name> --dir impact",
		"dex trace NewServer --dir impact",
		"dex trace '(*Server).Run' --dir impact --max-depth 4")
	pkg := fs.String("package", "", "package path filter (when the same name is defined in multiple packages)")
	maxDepth := fs.Int("max-depth", 0, "caller BFS depth (default 3, max 5)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("impact needs a <name> (path defaults to cwd)")
		}
		return fmt.Errorf("impact takes one <name> (got %d extra args)", len(rest)-1)
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
	out, err := s.GraphImpact(ctx, mcp.ImpactInput{
		Name:        rest[0],
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
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		return nil
	}
	fmt.Printf("blast-radius of %q — %d reachable caller(s) within depth %d", rest[0], out.Total, out.MaxDepth)
	if out.Truncated {
		fmt.Print(" (truncated)")
	}
	fmt.Print(":\n\n")
	for _, n := range out.Nodes {
		loc := n.Path
		if n.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", n.Path, n.StartLine)
		}
		fmt.Printf("  d%d  %s (%s) %s\n", n.Depth, n.QualifiedName, n.Kind, loc)
	}
	return nil
}

// cmdGrep is the front door for exact RE2 regex search (MCP: grep). It delegates
// to the same SearchGrep handler the MCP tool uses; the leading [path] is the
// project root (defaults to cwd) and --in narrows to a subdirectory.
func cmdGrep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grep", flag.ContinueOnError)
	setHelp(fs,
		"Exact RE2 regex search over project files (MCP: grep).",
		"dex grep [flags] [<path>] <pattern>",
		"dex grep 'func New'",
		"dex grep --ext go --in internal/mcp 'AddTool'")
	ext := fs.String("ext", "", "file extension filter without leading dot, e.g. go or ts")
	in := fs.String("in", "", "restrict to a subdirectory of the project")
	maxResults := fs.Int("max-results", 0, "maximum matches (default 50, max 200)")
	contextN := fs.Int("context", 0, "lines of context before/after each match (like grep -C), 0-10")
	fixed := fs.Bool("fixed", false, "match the pattern literally (like grep -F), not as a regex")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		if len(rest) == 0 {
			return fmt.Errorf("grep needs a <pattern> (path defaults to cwd); quote patterns with spaces")
		}
		return fmt.Errorf("grep takes one <pattern> (got %d); quote patterns with spaces", len(rest))
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
	out, err := s.SearchGrep(ctx, mcp.SearchGrepInput{
		Pattern:     rest[0],
		Path:        *in,
		Ext:         *ext,
		MaxResults:  *maxResults,
		Context:     *contextN,
		Fixed:       *fixed,
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
	for _, m := range out.Matches {
		if len(m.Before) == 0 && len(m.After) == 0 {
			fmt.Printf("%s:%d: %s\n", m.Path, m.Line, m.Content)
			continue
		}
		// Context view: a grep -C-style block with the match line marked ':'
		// and context lines marked '-'.
		for j, l := range m.Before {
			fmt.Printf("%s-%d- %s\n", m.Path, m.Line-len(m.Before)+j, l)
		}
		fmt.Printf("%s:%d: %s\n", m.Path, m.Line, m.Content)
		for j, l := range m.After {
			fmt.Printf("%s-%d- %s\n", m.Path, m.Line+1+j, l)
		}
		fmt.Println("--")
	}
	if out.Truncated {
		fmt.Fprintf(os.Stderr, "\n(%d matches, truncated)\n", out.Total)
	}
	return nil
}

// cmdShell runs a command and returns compressed output (MCP: shell), delegating
// to ShellRun so the CLI and tool share one compression policy. Flags must
// precede the command; every token after the first non-flag is the verbatim
// command, so its own flags pass through untouched. The child's exit code
// becomes dex's exit code.
func cmdShell(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	setHelp(fs,
		"Run a command with compressed output (MCP: shell).",
		"dex shell [flags] <command...>",
		"dex shell go test ./...",
		"dex shell --raw git status")
	cwd := fs.String("cwd", "", "working directory (default: current directory)")
	raw := fs.Bool("raw", false, "skip compression, return full output")
	timeout := fs.Int("timeout", 0, "per-call timeout in seconds (default 60, max 600; 0 = default)")
	format := fs.String("format", "text", "output format: text | json")
	// No reorderFlags here: the command tail may carry its own flags, so we let
	// flag.Parse stop at the first non-flag token and forward the remainder.
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("shell needs a <command>")
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.ShellRun(ctx, mcp.ShellInput{
		Command:     strings.Join(rest, " "),
		Cwd:         *cwd,
		Raw:         *raw,
		TimeoutSecs: *timeout,
	})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Print(out.Output)
	if !strings.HasSuffix(out.Output, "\n") {
		fmt.Println()
	}
	// Propagate the child's exit code so `dex shell make build && …` chains work.
	if out.ExitCode != 0 {
		os.Exit(out.ExitCode)
	}
	return nil
}
