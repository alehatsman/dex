// Package gitlog is the shared, dependency-free primitive for mining a
// repo's commit log: hash + subject + changed-file list per non-merge
// commit. It exists so both internal/graph (co-change edge mining, #212)
// and internal/eval (golden-set generation) can walk git history without
// creating an import cycle — internal/eval already imports internal/graph,
// so internal/graph cannot import internal/eval back. gitlog sits below
// both: only os/exec and internal/gitenv.
package gitlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/alehatsman/dex/internal/gitenv"
)

// Commit is one non-merge commit's short hash, subject, and changed-file
// list, as parsed from `git log --name-only`.
type Commit struct {
	ShortHash string
	Subject   string
	Files     []string
}

// Collect runs `git log --no-merges --name-only --relative` against root and
// parses up to max commits. --relative both restricts output to files under
// root and rebases paths to be root-relative, so a call against an
// index_subdir yields paths matching how the index records them.
//
// Output shape per commit block:
//
//	<hash>\x00<subject>      ← metadata line (the only line carrying a NUL)
//	                         ← blank
//	path/one.go              ← changed files, one per line
//	path/two.go
//
// Metadata lines are detected by the NUL they carry; every non-empty line in
// between is a changed file of the current commit. This avoids sentinel-
// placement fragility.
//
// Returns (nil, nil) — not an error — when root has no git history to read
// (git exits nonzero with empty output, e.g. a repo with zero commits or no
// .git at all): callers treat that as "nothing to mine", not a failure. A
// genuine failure to run git at all (binary missing, context
// cancelled/deadline exceeded) is always returned as a real error, even
// though its output also happens to be empty — callers that care about the
// difference (e.g. preserving previously-mined state on failure rather than
// treating it as "history is now empty") need that distinction.
func Collect(ctx context.Context, root string, max int) ([]Commit, error) {
	args := []string{
		"log",
		"--no-merges",
		"--format=%H%x00%s",
		"--name-only",
		"--relative",
		fmt.Sprintf("--max-count=%d", max),
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = gitenv.Current() // prevent hook-injected GIT_DIR from redirecting to wrong repo (#716)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, err // git binary missing/unrunnable — a real failure
		}
		if len(out) == 0 {
			return nil, nil // clean nonzero exit, no output: no git history to read
		}
		return nil, err
	}

	var recs []Commit
	var cur *Commit
	for _, raw := range bytes.Split(out, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		if i := bytes.IndexByte(line, 0); i >= 0 {
			// Metadata line: starts a new commit record. Subject may be
			// empty (e.g. --allow-empty-message) — kept as-is; callers that
			// need non-empty query text (internal/eval) filter for that
			// themselves, since a co-change miner only needs .Files.
			hash := strings.TrimSpace(string(line[:i]))
			subject := strings.TrimSpace(string(line[i+1:]))
			if len(hash) < 8 {
				cur = nil
				continue
			}
			recs = append(recs, Commit{ShortHash: hash[:8], Subject: subject})
			cur = &recs[len(recs)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if f := strings.TrimSpace(string(line)); f != "" {
			cur.Files = append(cur.Files, f)
		}
	}
	return recs, nil
}
