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

// cmdReview is the front door for per-hunk PR intelligence (MCP: review_diff,
// DEX_EXPERT; query kind=review covers the everyday working-tree case on the
// default surface). It composes the diff with callers, tests, churn, author
// history, and notes per hunk so a reviewer spends budget on judgment, not
// context assembly. With no selector it defaults to the last commit
// (HEAD~1..HEAD).
func cmdReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review_diff", flag.ContinueOnError)
	setHelp(fs,
		"Per-hunk intelligence for a diff or PR (MCP: review_diff, DEX_EXPERT). Composes callers, tests, churn, author history, and notes.",
		"dex review_diff [flags] [<path>]",
		"dex review_diff --ref HEAD~3..HEAD",
		"dex review_diff --branch feat/foo",
		"dex review_diff --worktree",
		"dex review_diff --pr 42 --compact")
	ref := fs.String("ref", "", "git range ('HEAD~3..HEAD') or a single ref (vs HEAD); defaults to HEAD~1..HEAD")
	branch := fs.String("branch", "", "branch name; reviews what it adds since diverging from --base")
	pr := fs.Int("pr", 0, "GitHub PR number (resolved via the gh CLI)")
	worktree := fs.Bool("worktree", false, "review uncommitted working-tree changes (git diff HEAD)")
	base := fs.String("base", "", "base branch for --branch/--pr comparison (default 'main')")
	compact := fs.Bool("compact", false, "drop low-risk hunks, returning only medium/high-risk ones")
	k := fs.Int("k", 0, "max callers and notes per symbol (default 8, max 30)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 0 {
		return fmt.Errorf("review_diff takes no positional args besides an optional path (got %d extra)", len(rest))
	}
	// CLI convenience: no selector → review the last commit. (--worktree opts
	// into the uncommitted working tree instead; MCP defaults there, #137.)
	if *ref == "" && *branch == "" && *pr == 0 && !*worktree {
		*ref = "HEAD~1..HEAD"
	}

	base2, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(path, base2)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base2)
	out, err := s.Review(ctx, mcp.ReviewInput{
		Ref:         *ref,
		Branch:      *branch,
		PR:          *pr,
		Worktree:    *worktree,
		Base:        *base,
		Compact:     *compact,
		K:           *k,
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
	renderReviewText(out)
	return nil
}

// renderReviewText prints the human view of a review bundle.
func renderReviewText(out mcp.ReviewOutput) {
	fmt.Printf("review of %s — %d hunk(s) across %d file(s)", out.Range, out.TotalHunks, len(out.Files))
	if out.Truncated {
		fmt.Print(" (truncated)")
	}
	fmt.Print(":\n")
	for _, f := range out.Files {
		fmt.Printf("\n─── %s (%s)", f.Path, f.Status)
		if f.OldPath != "" {
			fmt.Printf("  ⟵ %s", f.OldPath)
		}
		fmt.Println()
		if f.LastCommit != "" {
			fmt.Printf("    last: %s  (%s)\n", f.LastCommit, f.LastAuthor)
		}
		if f.Churn30d > 0 {
			fmt.Printf("    churn(30d): %d commits", f.Churn30d)
			if len(f.AuthorHistory) > 0 {
				fmt.Printf("  authors: %v", f.AuthorHistory)
			}
			fmt.Println()
		}
		if len(f.Tests) > 0 {
			fmt.Printf("    tests: %v\n", f.Tests)
		}
		if f.NearestDoc != "" {
			fmt.Printf("    doc: %s\n", f.NearestDoc)
		}
		for _, h := range f.Hunks {
			fmt.Printf("    @@ %d,%d → %d,%d  [%s] %s\n",
				h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.RiskTier, h.RiskReason)
			if h.Heading != "" {
				fmt.Printf("       in: %s\n", h.Heading)
			}
			for _, sym := range h.SymbolsTouched {
				exp := ""
				if sym.Exported {
					exp = " (exported)"
				}
				callers := ""
				if sym.CallerCount > 0 {
					callers = fmt.Sprintf(" — %d callers", sym.CallerCount)
				}
				fmt.Printf("       • %s%s%s\n", sym.Name, exp, callers)
			}
		}
	}
}
