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

// cmdQuery is the CLI's single read verb (#849 CLI collapse, specs/query-
// unification.md): the one front door onto (*Server).Query, the same
// dispatcher MCP's default `query` tool and the REST `/query` route already
// use. It replaces the CLI's former per-lane verbs (ask, search, read, locate,
// trace, grep, review_diff, status, repo_map, check, refs, cohort, and `graph
// deps`) — each was a named door to a lane `query` already serves; keeping
// them as separate verbs was the CLI half of the three-transport drift the
// spec exists to close.
//
// The flag surface mirrors QueryInput field-for-field (kind/want/to/budget/
// project-root, plus k/context/fixed already on the request shape) — no
// per-verb bespoke flags (--ext, --package, --max-depth, --explain, --ref,
// --mode=summary, …). That is a deliberate reduction, not an oversight: those
// were CLI-only (or DEX_EXPERT-tool-only) refinements that never made it into
// the unified shape. They are not deleted from the binary — the underlying
// MCP tools (search/trace/grep/read/review_diff/…, all DEX_EXPERT) still carry
// them — only no longer reachable from this collapsed CLI front door. See the
// issue for the full list of what a query-based front door does and doesn't
// reach.
func cmdQuery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	setHelp(fs,
		"The read verb — one call to read the codebase intelligence (mirrors the MCP `query` tool and the REST /query route).",
		"dex query [flags] [<path>] <input...>",
		`dex query "how are edits debounced?"`,
		"dex query internal/mcp/server.go",
		"dex query internal/mcp/server.go:829",
		"dex query NewServer --kind=callers",
		"dex query --kind=search \"retry logic\"",
		"dex query --kind=assemble \"add rate limiting\"",
		"dex query --kind=check internal/mcp/server.go:47 internal/mcp/server.go:100:nonexistent",
		"dex query --kind=refs --want=implementations toolSurface",
		"dex query --kind=cohort toolSurface",
		"dex query --kind=deps internal/mcp",
	)
	kind := fs.String("kind", "", "force the lane instead of inferring it from input shape: read|grep|locate|symbol|callers|callees|impact|path|search|editing|assemble|architecture|packages|orient|review|check|refs|cohort|deps|status")
	want := fs.String("want", "", "facet within the lane, e.g. signatures|map|skeleton|lines:N-M (read), callers|callees|impact|path (symbol), answer|assemble (prose), references|implementations|supertypes|subtypes (refs)")
	to := fs.String("to", "", "destination symbol for the graph 'path' facet")
	budget := fs.Int("budget", 0, "context-token budget; response reports cost.budget_left")
	projectRoot := fs.String("project-root", "", "explicit absolute project/worktree root (overrides the leading <path> positional)")
	k := fs.Int("k", 0, "max results per lane")
	contextN := fs.Int("context", 0, "grep lane: lines of context per match (0-10)")
	fixed := fs.Bool("fixed", false, "grep lane: treat the pattern as a literal string, not a regex")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	// With an explicit --project-root, don't also peel a leading path
	// positional off the input: that heuristic exists to *find* the project
	// root from an unqualified invocation, which --project-root already
	// settled. Skipping it matters most for --kind=deps, whose target is
	// often itself a relative package directory (`cmd/dex`) that would
	// otherwise be silently swallowed as a redundant path positional (#849).
	rest := fs.Args()
	root := *projectRoot
	if root == "" {
		root, rest = splitProjectArg(rest)
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(root, base)
	if err != nil {
		return err
	}

	in, err := buildQueryInput(*kind, *want, *to, *budget, *k, *contextN, *fixed, p.Root, rest)
	if err != nil {
		return err
	}

	s, _ := newServerFromEnv(base)
	out, err := s.Query(ctx, in)
	if err != nil {
		return err
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
		if queryFailed(out) {
			os.Exit(1)
		}
		return nil
	}
	renderQueryText(out)
	if queryFailed(out) {
		os.Exit(1)
	}
	return nil
}

// buildQueryInput constructs the QueryInput cmdQuery sends to (*Server).Query
// from already-parsed flag values and positional args. Pure/no I/O so the
// CLI's flag→request mapping is unit-testable without a server (see
// TestCLIQueryRouterAccuracy, the CLI-flag variant of #849's router-accuracy
// corpus, spec Validation).
func buildQueryInput(kind, want, to string, budget, k, contextN int, fixed bool, projectRoot string, rest []string) (mcp.QueryInput, error) {
	in := mcp.QueryInput{
		Kind: kind, Want: want, To: to, Budget: budget,
		K: k, Context: contextN, Fixed: fixed,
		ProjectRoot: projectRoot,
	}
	// check is query's one documented non-scalar-input exception (spec's
	// resolved open question #2): every remaining positional is a claim, not
	// words of one joined input string.
	if strings.EqualFold(strings.TrimSpace(kind), "check") {
		if len(rest) == 0 {
			return in, fmt.Errorf("query --kind=check needs at least one <ref> (file:line or file:line:symbol)")
		}
		claims := make([]mcp.ClaimRef, len(rest))
		for i, r := range rest {
			claims[i] = mcp.ClaimRef{Ref: r}
		}
		in.Claims = claims
	} else {
		// Every other lane takes one input string; unquoted multi-word prose
		// (a question, a grep pattern with spaces) joins the same way `ask`/
		// `search` used to, so quoting stays optional.
		in.Input = strings.Join(rest, " ")
	}
	return in, nil
}

// queryFailed reports whether out represents a verification failure worth a
// non-zero exit — today only kind=check.
func queryFailed(out mcp.QueryOutput) bool {
	if out.Result.Check == nil {
		return false
	}
	for _, r := range out.Result.Check.Results {
		if checkStatusFailed(r.Status) {
			return true
		}
	}
	return false
}

// checkStatusFailed reports whether one check claim's status counts as a
// verification failure — the single source of truth for `dex query
// --kind=check`'s non-zero exit code, consulted before the text/JSON render
// split so both output modes agree (ported from the former cmd/dex/check.go).
func checkStatusFailed(status string) bool {
	switch status {
	case "moved", "gone", "no_file", "parse_error":
		return true
	}
	return false
}
