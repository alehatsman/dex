package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
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

// chunksInRange returns every named chunk whose span overlaps [startLine,
// endLine], smallest-span first. In-memory counterpart to store.ChunksInRange
// for the time-travel path (#215), where chunks come from an in-memory parse
// of historical content rather than the index.
func chunksInRange(chunks []chunk.Chunk, startLine, endLine int) []chunk.Chunk {
	var out []chunk.Chunk
	for _, c := range chunks {
		if c.Name != "" && c.StartLine <= endLine && c.EndLine >= startLine {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return (out[i].EndLine - out[i].StartLine) < (out[j].EndLine - out[j].StartLine)
	})
	return out
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, maxDiffBytes))
	truncated := len(data) >= maxDiffBytes
	// Kill ensures the process exits promptly if we stopped reading early;
	// it is a no-op when the process already exited naturally.
	_ = cmd.Process.Kill()
	waitErr := cmd.Wait()
	if readErr != nil {
		return "", readErr
	}
	// A genuinely bad ref (or non-repo root) exits non-zero with nothing on
	// stdout — that must not read as "no changes" (#231/#12). Only trust
	// Wait's error when the process wasn't killed by US: a truncated read means
	// we killed a still-running-but-healthy process at the byte cap, and
	// cctx.Err() != nil means the context deadline (reviewGitTimeout) or an
	// upstream cancellation killed it — a large-but-legitimate diff that's
	// still streaming when the timeout fires must degrade to its partial
	// output exactly like the old code did, not read as a command failure
	// (both cases make Wait's error just "signal: killed", never a real exit
	// code).
	if !truncated && cctx.Err() == nil && waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return "", fmt.Errorf("git diff %s: %s", rng, msg)
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
