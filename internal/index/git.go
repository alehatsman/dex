package index

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/store"
)

// GitIndexer indexes git commits as searchable chunks of kind git_commit.
// Each commit becomes one chunk with path "git:<8-char-hash>" containing
// the commit metadata and changed-file list. Embeddings are generated in
// batches and stored via UpsertMany.
//
// Incremental: the last-indexed HEAD is persisted in the store's meta table.
// On next run only commits newer than that HEAD are indexed. Force-push
// detection: if the stored HEAD is no longer an ancestor of the current
// HEAD, all existing git_commit chunks are wiped and the full history is
// re-indexed (up to MaxCommits).
type GitIndexer struct {
	Root   string
	St     *store.Store
	Embed  embed.Embedder
	// MaxCommits caps how many commits are indexed in a full (non-incremental)
	// run. 0 uses the default of 500.
	MaxCommits int
}

const gitIndexBatchSize = 50
const gitIndexDefaultMax = 500

// Run performs the git commit indexing pass. It is safe to call from
// cmdIndex as Phase 3 after the graph phase.
func (g *GitIndexer) Run(ctx context.Context) error {
	max := g.MaxCommits
	if max <= 0 {
		max = gitIndexDefaultMax
	}

	last, err := g.St.GitLastIndexed(ctx)
	if err != nil {
		return fmt.Errorf("git index: read last commit: %w", err)
	}

	// If we have a stored last commit, verify it is still an ancestor of
	// HEAD (detects force-push / history rewrite). If it isn't, wipe all
	// existing git_commit chunks and re-index from scratch.
	if last != "" {
		if !g.isAncestor(last) {
			if err := g.St.DeletePathPrefix(ctx, "git:"); err != nil {
				return fmt.Errorf("git index: wipe on force-push: %w", err)
			}
			last = ""
		}
	}

	commits, err := g.collectCommits(ctx, last, max)
	if err != nil {
		return fmt.Errorf("git index: collect commits: %w", err)
	}
	if len(commits) == 0 {
		return nil
	}

	now := time.Now()
	for start := 0; start < len(commits); start += gitIndexBatchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := start + gitIndexBatchSize
		if end > len(commits) {
			end = len(commits)
		}
		batch := commits[start:end]

		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.content
		}
		vecs, err := g.Embed.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("git index: embed batch %d: %w", start/gitIndexBatchSize+1, err)
		}

		rows := make([]store.PendingChunk, len(batch))
		for i, c := range batch {
			rows[i] = store.PendingChunk{
				Path:       "git:" + c.shortHash,
				Kind:       chunk.KindGitCommit,
				Name:       c.subject,
				StartLine:  0,
				EndLine:    0,
				ContentSHA: sha1Hex(c.content),
				Content:    c.content,
				Vec:        vecs[i],
			}
		}
		if err := g.St.UpsertMany(ctx, rows, now); err != nil {
			return fmt.Errorf("git index: upsert batch %d: %w", start/gitIndexBatchSize+1, err)
		}
	}

	// Record the new HEAD so the next run is incremental.
	head, err := g.headHash()
	if err != nil {
		return fmt.Errorf("git index: read HEAD: %w", err)
	}
	return g.St.SetGitLastIndexed(ctx, head)
}

type gitCommit struct {
	shortHash string
	subject   string
	content   string
}

// collectCommits runs git log and parses its output into gitCommit records.
// If last is non-empty, only commits not reachable from last are returned
// (i.e. commits added since last). Otherwise returns up to max commits.
func (g *GitIndexer) collectCommits(ctx context.Context, last string, max int) ([]gitCommit, error) {
	// NUL-delimited fields per commit:  hash NUL authorName NUL date NUL subject NUL body
	// After each record we print a sentinel line so we can split records.
	const sentinel = "---DEX-GIT-SEP---"
	format := "%H%x00%an%x00%ad%x00%s%x00%b%x00" + sentinel

	args := []string{
		"log",
		"--format=" + format,
		"--date=short",
		"--name-only",
		fmt.Sprintf("--max-count=%d", max),
	}
	if last != "" {
		args = append(args, last+"..HEAD")
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Root
	out, err := cmd.Output()
	if err != nil {
		// No commits or empty repo → treat as success with zero results.
		if len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}

	return parseGitLog(out, sentinel), nil
}

func parseGitLog(data []byte, sentinel string) []gitCommit {
	// Each record ends with "\n---DEX-GIT-SEP---\n"
	records := bytes.Split(data, []byte("\n"+sentinel+"\n"))
	var commits []gitCommit
	for _, rec := range records {
		rec = bytes.TrimSpace(rec)
		if len(rec) == 0 {
			continue
		}
		// The record is: hash NUL author NUL date NUL subject NUL body NUL
		// followed by a blank line then file list.
		parts := bytes.SplitN(rec, []byte{0}, 6)
		if len(parts) < 5 {
			continue
		}
		hash := strings.TrimSpace(string(parts[0]))
		author := strings.TrimSpace(string(parts[1]))
		date := strings.TrimSpace(string(parts[2]))
		subject := strings.TrimSpace(string(parts[3]))
		rest := string(parts[4])
		if len(parts) > 5 {
			rest = string(parts[5])
		}

		// rest = body NUL (then possibly blank line + file list)
		// The body and file list are separated by the trailing NUL from format.
		bodyAndFiles := strings.SplitN(rest, "\x00", 2)
		body := strings.TrimSpace(bodyAndFiles[0])
		var files []string
		if len(bodyAndFiles) > 1 {
			for _, f := range strings.Split(strings.TrimSpace(bodyAndFiles[1]), "\n") {
				f = strings.TrimSpace(f)
				if f != "" {
					files = append(files, f)
				}
			}
		}

		if hash == "" || subject == "" {
			continue
		}

		content := buildCommitContent(hash, author, date, subject, body, files)
		commits = append(commits, gitCommit{
			shortHash: hash[:8],
			subject:   subject,
			content:   content,
		})
	}
	return commits
}

func buildCommitContent(hash, author, date, subject, body string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hash: %s\n", hash)
	fmt.Fprintf(&b, "Author: %s\n", author)
	fmt.Fprintf(&b, "Date: %s\n", date)
	fmt.Fprintf(&b, "Subject: %s\n", subject)
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if len(files) > 0 {
		b.WriteString("\nFiles changed:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}
	return b.String()
}

// isAncestor checks whether hash is an ancestor of HEAD using
// git merge-base --is-ancestor.
func (g *GitIndexer) isAncestor(hash string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", hash, "HEAD")
	cmd.Dir = g.Root
	return cmd.Run() == nil
}

// headHash returns the full hash of HEAD.
func (g *GitIndexer) headHash() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = g.Root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sha1Hex(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))
}
