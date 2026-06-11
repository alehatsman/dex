package index

import (
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
	Root  string
	St    *store.Store
	Embed embed.Embedder
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
			}
		}

		// Lean / BM25-only mode (DEX_EMBED_ENGINE=none) wires no embedder:
		// upsert commit chunks with nil vectors so they stay FTS-searchable
		// without corrupting a vector index (mirrors the file indexer).
		if g.Embed != nil {
			texts := make([]string, len(batch))
			for i, c := range batch {
				texts[i] = c.content
			}
			vecs, err := g.Embed.Embed(ctx, texts)
			if err != nil {
				return fmt.Errorf("git index: embed batch %d: %w", start/gitIndexBatchSize+1, err)
			}
			for i := range rows {
				rows[i].Vec = vecs[i]
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
	// Machine-readable layout, robust to multi-line bodies and odd paths:
	//   - RS (0x1e) starts each commit record (split commits on it).
	//   - US (0x1f) separates the five fixed fields; body is last so any
	//     newlines it contains stay inside one field.
	//   - -z + --name-only makes the changed-file list NUL-separated and
	//     trail the body field, so paths with spaces/newlines survive.
	// Only a literal RS/US byte inside a commit message could fool this,
	// which effectively never happens.
	format := "%x1e%H%x1f%an%x1f%ad%x1f%s%x1f%b%x1f"

	args := []string{
		"log",
		"-z",
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

	return parseGitLog(out), nil
}

const (
	gitRecordSep = "\x1e" // RS — starts each commit record
	gitFieldSep  = "\x1f" // US — separates the fixed fields
)

// parseGitLog parses the RS/US/-z layout emitted by collectCommits into
// gitCommit records. Each record holds five US-separated fields (hash,
// author, date, subject, body) followed by the NUL-separated changed-file
// list. Records with no hash or subject are skipped.
func parseGitLog(data []byte) []gitCommit {
	var commits []gitCommit
	for _, rec := range strings.Split(string(data), gitRecordSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		fields := strings.SplitN(rec, gitFieldSep, 6)
		if len(fields) < 5 {
			continue
		}
		hash := strings.TrimSpace(fields[0])
		author := strings.TrimSpace(fields[1])
		date := strings.TrimSpace(fields[2])
		subject := strings.TrimSpace(fields[3])
		body := strings.TrimSpace(fields[4])

		var files []string
		if len(fields) == 6 {
			for _, f := range strings.Split(fields[5], "\x00") {
				if f = strings.TrimSpace(f); f != "" {
					files = append(files, f)
				}
			}
		}

		if hash == "" || subject == "" {
			continue
		}
		short := hash
		if len(short) > 8 {
			short = short[:8]
		}
		commits = append(commits, gitCommit{
			shortHash: short,
			subject:   subject,
			content:   buildCommitContent(hash, author, date, subject, body, files),
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
