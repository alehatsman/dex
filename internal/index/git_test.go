package index

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chunk"
	"github.com/alehatsman/dex/internal/store"
)

// initGitRepo creates a throwaway git repo at dir with deterministic
// identity so commit hashes/output are stable across machines.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Tester")
	run("config", "user.email", "t@example.com")
}

func gitCommitFile(t *testing.T, dir, file, content, subject, body string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), content)
	cmd := exec.Command("git", "add", file)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	msg := subject
	if body != "" {
		msg = subject + "\n\n" + body
	}
	cmd = exec.Command("git", "commit", "-q", "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00", "GIT_COMMITTER_DATE=2026-01-01T00:00:00",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

// TestParseGitLogRealRepo exercises collectCommits against a real repo so
// the parser is validated against git's actual --name-only output, not an
// assumed byte layout. It pins the P1 fix: multi-line body must survive and
// the changed-file list must be captured.
func TestParseGitLogRealRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	gitCommitFile(t, dir, "alpha.go", "package a\n", "add alpha", "")
	gitCommitFile(t, dir, "beta.go", "package b\n",
		"add beta with details",
		"This body has\nmultiple lines\nof explanation.")

	g := &GitIndexer{Root: dir}
	commits, err := g.collectCommits(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("collectCommits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("want 2 commits, got %d: %+v", len(commits), commits)
	}

	// commits[0] is newest (beta).
	beta := commits[0]
	if beta.subject != "add beta with details" {
		t.Errorf("beta subject = %q", beta.subject)
	}
	for _, want := range []string{"multiple lines", "beta.go", "add beta with details"} {
		if !strings.Contains(beta.content, want) {
			t.Errorf("beta.content missing %q\n--- content ---\n%s", want, beta.content)
		}
	}
	if strings.Contains(beta.content, "alpha.go") {
		t.Errorf("beta.content leaked alpha.go (record boundary bug)\n%s", beta.content)
	}

	alpha := commits[1]
	if alpha.subject != "add alpha" {
		t.Errorf("alpha subject = %q", alpha.subject)
	}
	if !strings.Contains(alpha.content, "alpha.go") {
		t.Errorf("alpha.content missing alpha.go\n%s", alpha.content)
	}
}

// TestGitIndexerLeanNoEmbedder proves the git lane survives lean /
// BM25-only mode (nil embedder): no nil-pointer panic, and commit chunks
// land FTS-searchable with nil vectors. Regression for the P0 panic at
// git.go's Embed.Embed call.
func TestGitIndexerLeanNoEmbedder(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	gitCommitFile(t, dir, "alpha.go", "package a\n", "add alpha widget", "")

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g := &GitIndexer{Root: dir, St: st, Embed: nil} // lean: no embedder
	if err := g.Run(context.Background()); err != nil {
		t.Fatalf("GitIndexer.Run (lean): %v", err)
	}

	hits, err := st.Search(context.Background(), nil, "widget", 10)
	if err != nil {
		t.Fatalf("BM25 search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no BM25 hits for commit subject — commit chunk not indexed in lean mode")
	}
}

// TestPruneUnseenPreservesGitCommits proves a no-change re-index (which
// bumps file chunks then prunes stale rows) does not wipe synthetic
// git_commit chunks. Regression for the P0 data-loss bug where PruneUnseen
// deleted the entire commit-search corpus.
func TestPruneUnseenPreservesGitCommits(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	stamp := time.Now()
	rows := []store.PendingChunk{
		{Path: "git:abc12345", Kind: chunk.KindGitCommit, Name: "old commit",
			ContentSHA: "sha-git", Content: "Subject: old commit\n"},
		{Path: "src/a.go", Kind: "code", Name: "Foo",
			ContentSHA: "sha-code", Content: "func Foo() {}"},
	}
	if err := st.UpsertMany(ctx, rows, stamp); err != nil {
		t.Fatal(err)
	}

	// Prune with a cutoff past both rows' last_seen_at: the file walk never
	// re-touched either, so both look stale.
	if _, err := st.PruneUnseen(ctx, stamp.Add(time.Hour)); err != nil {
		t.Fatalf("PruneUnseen: %v", err)
	}

	git, err := st.Search(ctx, nil, "commit", 10)
	if err != nil {
		t.Fatal(err)
	}
	foundGit := false
	for _, h := range git {
		if strings.HasPrefix(h.Path, "git:") {
			foundGit = true
		}
	}
	if !foundGit {
		t.Error("git_commit chunk was pruned — commit corpus lost on re-index")
	}
	// Sanity: the non-git chunk SHOULD have been pruned, proving the cutoff bit.
	code, _ := st.Search(ctx, nil, "Foo", 10)
	for _, h := range code {
		if h.Path == "src/a.go" {
			t.Error("stale code chunk survived prune — test cutoff ineffective")
		}
	}
}
