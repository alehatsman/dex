package mcp

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/gitenv"
)

// gitShowFile retrieves the raw content of path at the given git ref.
func gitShowFile(ctx context.Context, root, ref, path string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root, "show", ref+":"+path) //nolint:gosec // ref validated by reValidRef; path is project-relative from diff output
	cmd.Env = gitenv.Current()
	return cmd.Output()
}

// chunkAtLine returns the most specific chunk whose span contains line.
// Mirrors the store.ChunkAt SQL (ORDER BY end_line-start_line ASC LIMIT 1).
func chunkAtLine(chunks []chunk.Chunk, line int) (chunk.Chunk, bool) {
	best, found := chunk.Chunk{}, false
	bestSpan := -1
	for _, c := range chunks {
		if c.StartLine <= line && line <= c.EndLine {
			span := c.EndLine - c.StartLine
			if !found || span < bestSpan {
				best, bestSpan, found = c, span, true
			}
		}
	}
	return best, found
}

// ─── range resolution + git helpers ──────────────────────────────────────

// resolveReviewRange turns the input selector into a git range token for
// `git diff`. Ref wins, then Branch, then PR (which resolves to a branch), then
// Worktree (bare HEAD → `git diff HEAD`, the uncommitted working tree, #137).
func resolveReviewRange(ctx context.Context, root string, in ReviewInput) (rng, status, hint string) {
	base := strings.TrimSpace(in.Base)
	if base == "" {
		base = "main"
	}
	if !reValidRef.MatchString(base) {
		return "", "error", fmt.Sprintf("invalid base ref %q", base)
	}

	if ref := strings.TrimSpace(in.Ref); ref != "" {
		if !reValidRef.MatchString(ref) {
			return "", "error", fmt.Sprintf("invalid ref %q — only alphanumeric, ~^:./_@{}- characters allowed", ref)
		}
		if !strings.Contains(ref, "..") {
			ref += "..HEAD" // single ref → ref..HEAD
		}
		return ref, "ok", ""
	}

	if br := strings.TrimSpace(in.Branch); br != "" {
		if !reValidRef.MatchString(br) {
			return "", "error", fmt.Sprintf("invalid branch %q", br)
		}
		return base + "..." + br, "ok", "" // symmetric: what the branch adds since divergence
	}

	// PR: resolve the head branch via gh, then review it like a branch.
	if in.PR != 0 {
		head := ghPRHeadBranch(ctx, root, in.PR)
		if head == "" {
			return "", "not-found", fmt.Sprintf("could not resolve PR #%d head branch — needs the `gh` CLI, a GitHub remote, and a fetched head", in.PR)
		}
		if !reValidRef.MatchString(head) {
			return "", "error", fmt.Sprintf("PR #%d head branch %q has unexpected characters", in.PR, head)
		}
		return base + "..." + head, "ok", ""
	}

	// Worktree: bare HEAD so gitDiffUnified runs `git diff HEAD` — the working
	// tree (staged + unstaged) vs HEAD. Ends at HEAD ⇒ no time-travel (#137).
	if in.Worktree {
		return "HEAD", "ok", ""
	}

	return "", "error", "review needs one of: ref, branch, pr, or worktree"
}

// rangeEndsAtHEAD reports whether a git range's right-hand side is HEAD (the
// state the index reflects). resolveReviewRange always emits a range containing
// ".." / "...", so the side after the last ".." is the head; an empty head
// (e.g. "ref..") defaults to HEAD in git.
func rangeEndsAtHEAD(rng string) bool {
	head := rng
	if i := strings.LastIndex(rng, ".."); i >= 0 {
		head = rng[i+2:]
	}
	head = strings.TrimSpace(head)
	return head == "" || head == "HEAD" || head == "@"
}

// gitDiffUnified runs `git diff --unified=0 <range>` in root and returns the raw
// unified diff. Zero context keeps hunks tight around the actual change.
// Output is capped at maxDiffBytes to prevent OOM on auto-generated or vendored
// diffs; the display caps in the review verb fire on the parsed hunks afterwards.
func gitDiffUnified(ctx context.Context, root, rng string) (string, error) {
	const maxDiffBytes = 4 * 1024 * 1024 // 4 MB hard cap
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"diff", "--unified=0", "--no-color", "--end-of-options", rng) // #nosec G204 — rng validated by reValidRef
	cmd.Env = gitenv.Current()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, maxDiffBytes))
	// Kill ensures the process exits promptly if we stopped reading early;
	// it is a no-op when the process already exited naturally.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if readErr != nil {
		return "", readErr
	}
	return string(data), nil
}

// gitChurnCount returns the number of commits touching path in the churn window
// (best-effort; 0 on any error or missing git).
func gitChurnCount(ctx context.Context, root, path string) int {
	if path == "" {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"rev-list", "--count", "--since="+reviewChurnWindow, "HEAD", "--", path) // #nosec G204
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

// gitUntrackedCount returns the number of untracked, non-ignored files in the
// working tree (best-effort; 0 on any error). They are invisible to
// `git diff HEAD`, so a clean-tree worktree review nudges about them (#137).
func gitUntrackedCount(ctx context.Context, root string) int {
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"ls-files", "--others", "--exclude-standard")
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// gitAuthorHistory returns the authors of the last 3 commits touching path,
// most-recent first (best-effort).
func gitAuthorHistory(ctx context.Context, root, path string) []string {
	if path == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", root,
		"log", "-3", "--format=%an", "--", path) // #nosec G204
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var authors []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			authors = append(authors, ln)
		}
	}
	return authors
}

// ghPRHeadBranch resolves a PR number to its head branch via the `gh` CLI.
// Best-effort: a missing binary, no repo, or any error yields "". Capped at the
// git timeout so a slow network never stalls review.
func ghPRHeadBranch(ctx context.Context, root string, pr int) string {
	if pr <= 0 {
		return ""
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, reviewGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "pr", "view", strconv.Itoa(pr),
		"--json", "headRefName", "--jq", ".headRefName")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
