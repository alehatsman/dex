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

// CLI verb front doors mirroring the MCP tool facade (#345/#354): find / trace
// / impact. They are thin wrappers over the existing commands — `dex search`,
// `dex graph …`, `dex knowledge`, etc. all keep working unchanged. map / read /
// ask are already top-level, so the six verbs (map find read trace impact ask)
// now match the default MCP surface one-for-one.

// cmdFind is the verb front door for code search (MCP: find). It runs the same
// fused semantic+symbol lane as `dex search semantic`.
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

mirrors MCP `+"`trace`"+`; re-dispatches to `+"`dex graph callers|callees|path`"+`.
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
	default:
		return fmt.Errorf("trace: --direction must be callers, callees, or path (got %q)", dir)
	}
}

// cmdImpact reports the transitive blast-radius of a symbol — every function
// reachable by following `calls` edges in the callers direction, tagged with
// hop depth (MCP: impact). No `dex graph` subcommand covered this before.
func cmdImpact(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	setHelp(fs,
		"Transitive blast-radius via callers (MCP: impact). Go-only today.",
		"dex impact [flags] [<path>] <name>",
		"dex impact NewServer",
		"dex impact '(*Server).Run' --max-depth 4")
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
