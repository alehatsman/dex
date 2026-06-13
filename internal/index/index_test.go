package index

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// fakeEmbedServer hashes each input into a deterministic 16-dim float
// vector. Same input → same vector; lets us assert reasonable retrieval
// behavior without a real model.
func fakeEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "no", 404)
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{Model: body.Model}
		for i, in := range body.Input {
			out.Data = append(out.Data, item{Index: i, Embedding: hashVec(in, 16)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func hashVec(s string, dim int) []float32 {
	out := make([]float32, dim)
	h := sha256.Sum256([]byte(s))
	for i := range dim {
		u := binary.LittleEndian.Uint32(h[(i*4)%len(h):])
		out[i] = float32(int32(u)) / float32(math.MaxInt32)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeIndexAll opts the temp project into indexing everything. Indexing
// is opt-in (.dex/config.yml index.include); without an include list
// the matcher skips every file, so these indexer tests would see an empty
// index. Mirrors the include = ["*"] escape used in the ignore tests.
func writeIndexAll(t *testing.T, dir string) {
	t.Helper()
	cfgDir := filepath.Join(dir, ".dex")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"),
		[]byte("index:\n  include: [\"*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexAndQuery(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	// A Go file with two top-level declarations.
	writeFile(t, filepath.Join(projDir, "alpha.go"), `package main

// Alpha is the first function.
func Alpha() string { return "alpha" }

// Beta is the second function.
func Beta() string { return "beta" }
`)
	// A Markdown file → line-window chunking.
	writeFile(t, filepath.Join(projDir, "README.md"),
		"# Project\n\nThis is a README that should be indexed via line-window chunks.\n"+
			"It has more than one paragraph.\n\nMore text.\n")
	// A secret-like file → should be skipped.
	writeFile(t, filepath.Join(projDir, "creds.txt"),
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n")
	// A node_modules directory → should be skipped by default ignore.
	writeFile(t, filepath.Join(projDir, "node_modules/foo/index.js"),
		"function ignored() {}\n")

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 8, 10*time.Second)
	ix := New(p, st, em, ig, Options{Verbose: false})

	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files < 2 {
		t.Errorf("expected at least 2 files indexed (alpha.go, README.md); got %d", stats.Files)
	}
	if stats.Dim != 16 {
		t.Errorf("dim: got %d, want 16", stats.Dim)
	}
	if stats.Chunks < 2 {
		t.Errorf("expected at least 2 chunks; got %d", stats.Chunks)
	}

	// Query — the embedded text for "Alpha" should be closer to alpha.go
	// than to README.md (since the same hash function is used).
	qvecs, err := em.Embed(ctx, []string{
		"// path: alpha.go\n// kind: function_declaration\n// Alpha is the first function.\nfunc Alpha() string { return \"alpha\" }",
	})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := st.Search(ctx, qvecs[0], "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("Search returned no hits")
	}
	if hits[0].Path != "alpha.go" {
		t.Errorf("top hit path: got %q, want alpha.go", hits[0].Path)
	}

	// Re-run: nothing should change in chunk count (idempotent).
	before := stats.Chunks
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	stats2, _ := st.Stats(ctx)
	if stats2.Chunks != before {
		t.Errorf("re-index changed chunk count: %d → %d", before, stats2.Chunks)
	}

	// Remove a file; re-index; chunk count should drop.
	if err := os.Remove(filepath.Join(projDir, "alpha.go")); err != nil {
		t.Fatal(err)
	}
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run #3: %v", err)
	}
	stats3, _ := st.Stats(ctx)
	if stats3.Chunks >= before {
		t.Errorf("expected chunk count to drop after removing alpha.go; got %d (was %d)", stats3.Chunks, before)
	}
}

// TestDuplicateContentChunksSurvive guards #434: two byte-identical chunks in
// one file used to collide on UNIQUE(path, content_sha1) and silently drop all
// but the last. The per-file dedup ordinal must give each a distinct
// content_sha1 so both survive indexing.
func TestDuplicateContentChunksSurvive(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	// indexDupSHAs builds a fresh index of a Go file holding n byte-identical
	// Dup() declarations and returns how many distinct content_sha1 rows the
	// store kept for it. Fresh projects (separate temp dirs) avoid any mtime
	// fast-path / prune interplay — each call exercises the chunk path cleanly.
	indexDupSHAs := func(n int) int {
		t.Helper()
		projDir := t.TempDir()
		cacheDir := t.TempDir()
		writeIndexAll(t, projDir)

		body := "package main\n"
		// Byte-identical source text — won't compile, but the indexer chunks
		// text, not a typechecked program, so the n chunks hash identically.
		for i := 0; i < n; i++ {
			body += "\nfunc Dup() string { return \"x\" }\n"
		}
		writeFile(t, filepath.Join(projDir, "dup.go"), body)

		ctx := context.Background()
		p, err := proj.Resolve(projDir, cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.EnsureCacheDir(); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(ctx, p.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		ig, err := ignore.New(p.Root)
		if err != nil {
			t.Fatal(err)
		}
		em := embed.New(srv.URL, "fake", 8, 10*time.Second)
		ix := New(p, st, em, ig, Options{Verbose: false})
		if err := ix.Run(ctx); err != nil {
			t.Fatalf("Run (n=%d): %v", n, err)
		}
		shas, err := st.ExistingSHAsBatch(ctx, []string{"dup.go"})
		if err != nil {
			t.Fatal(err)
		}
		return len(shas["dup.go"])
	}

	// Before #434's fix, byte-identical chunks collided on
	// UNIQUE(path, content_sha1) and all but the last were silently dropped on
	// UPSERT — so the distinct count would flatline regardless of n. With the
	// per-file dedup ordinal each duplicate gets a distinct content_sha1, so
	// the count must rise one-for-one with each added identical declaration.
	base := indexDupSHAs(1)
	if base < 1 {
		t.Fatalf("baseline indexed no chunks for dup.go")
	}
	for _, n := range []int{2, 3, 5} {
		if got, want := indexDupSHAs(n), base+(n-1); got != want {
			t.Errorf("n=%d identical chunks: got %d distinct content_sha1, want %d (baseline %d + %d duplicates)", n, got, want, base, n-1)
		}
	}
}

// TestNewlyIgnoredEviction makes sure that adding a path to
// .dexignore (or .gitignore) between runs evicts the chunks that
// were previously indexed under that path. Without explicit eviction
// the walker would simply skip the subtree on the next run and the
// stale chunks would live forever in the index.
func TestNewlyIgnoredEviction(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\nfunc Main() {}\n")
	writeFile(t, filepath.Join(projDir, "drafts/wip.go"),
		"package drafts\nfunc WIP() {}\n")

	ctx := context.Background()
	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(ctx, p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)
	em := embed.New(srv.URL, "fake", 8, 5*time.Second)
	ix := New(p, st, em, ig, Options{})

	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stats0, _ := st.Stats(ctx)
	if stats0.Files < 2 {
		t.Fatalf("expected both files indexed, got %d", stats0.Files)
	}

	// Add an ignore rule and reload the matcher.
	writeFile(t, filepath.Join(projDir, ".dexignore"), "drafts/\n")
	ig2, _ := ignore.New(p.Root)
	ix2 := New(p, st, em, ig2, Options{})
	if err := ix2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stats1, _ := st.Stats(ctx)
	if stats1.Files != 1 {
		t.Errorf("expected drafts/ to be evicted, got %d files in index", stats1.Files)
	}
}

// TestWorktreeCheckoutSkipped verifies that a git worktree checkout nested
// under the project root is not indexed. A worktree checkout is identified
// by a .git FILE (not directory) inside it.
func TestWorktreeCheckoutSkipped(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\nfunc Main() {}\n")

	// Simulate a git worktree checkout: a subdirectory with a .git FILE.
	wtDir := filepath.Join(projDir, ".worktrees", "feat", "my-feature")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wtDir, ".git"), "gitdir: /some/other/repo/.git/worktrees/my-feature\n")
	writeFile(t, filepath.Join(wtDir, "main.go"), "package main\nfunc WorktreeFunc() {}\n")

	ctx := context.Background()
	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(ctx, p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)
	ix := New(p, st, embed.New(srv.URL, "fake", 8, 5*time.Second), ig, Options{})

	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.Stats(ctx)
	if stats.Files != 1 {
		t.Errorf("expected 1 file indexed (worktree skipped), got %d", stats.Files)
	}
	// Verify no worktree paths appear in the index.
	paths, _ := st.CodeFilePaths(ctx)
	for p := range paths {
		if strings.Contains(p, ".worktrees") {
			t.Errorf("worktree path should not be indexed: %s", p)
		}
	}
}

// TestPruneAtSameMillisecond exercises the regression where two successive
// Run() calls completing inside the same millisecond used to share a
// last_seen_at value, defeating the strict-less-than PruneUnseen filter.
// With nanosecond timestamps each call must produce a distinct cutoff.
func TestPruneAtSameMillisecond(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	writeFile(t, filepath.Join(projDir, "a.go"),
		"package main\nfunc A() {}\n")
	writeFile(t, filepath.Join(projDir, "b.go"),
		"package main\nfunc B() {}\n")

	ctx := context.Background()
	p, _ := proj.Resolve(projDir, cacheDir)
	_ = p.EnsureCacheDir()
	st, _ := store.Open(ctx, p.DBPath)
	defer st.Close()
	ig, _ := ignore.New(p.Root)
	em := embed.New(srv.URL, "fake", 8, 5*time.Second)
	ix := New(p, st, em, ig, Options{})

	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(projDir, "a.go")); err != nil {
		t.Fatal(err)
	}
	// Re-run in the same millisecond as the first. With nanosecond
	// precision the second cutoff strictly succeeds the first, so the
	// stale chunks for a.go must be pruned.
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.Stats(ctx)
	if stats.Files != 1 {
		t.Errorf("expected 1 file after pruning a.go; got %d", stats.Files)
	}
}

func TestParallelWalkIndexesAllFiles(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	const fileCount = 64
	for i := 0; i < fileCount; i++ {
		writeFile(t, filepath.Join(projDir, fmt.Sprintf("f%02d.go", i)),
			fmt.Sprintf(`package main

func F%d() string { return "f%d" }
`, i, i))
	}

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 16, 10*time.Second)
	// Concurrency well above GOMAXPROCS to stress the channels.
	ix := New(p, st, em, ig, Options{Concurrency: 8})

	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != fileCount {
		t.Errorf("Files indexed = %d, want %d", stats.Files, fileCount)
	}
	if stats.Chunks < fileCount {
		t.Errorf("Chunks indexed = %d, want >= %d (one func per file)", stats.Chunks, fileCount)
	}
}

// TestMtimeFastPathEqualMtimeReindexes guards #439: a file whose mtime
// equals the previous run's last_indexed_at must NOT be skipped by the
// mtime fast-path. Filesystem mtimes are often second-granular, so a
// file edited in the same second the prior run started lands on exactly
// that boundary; a `<=` test would skip it forever, silently dropping
// the edit. The fix is a strict `<` so equal-mtime files fall to the
// slow path, where the SHA dedup re-confirms whether they changed.
func TestMtimeFastPathEqualMtimeReindexes(t *testing.T) {
	srv := fakeEmbedServer(t)
	defer srv.Close()

	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)
	fpath := filepath.Join(projDir, "a.go")
	writeFile(t, fpath, "package main\n\nfunc A() {}\n")

	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	em := embed.New(srv.URL, "fake", 8, 5*time.Second)
	ix := New(p, st, em, ig, Options{})

	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	before, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Edit the file to add a second declaration, then pin the stored
	// last_indexed_at to the file's actual on-disk mtime. Reading the
	// mtime back from the filesystem (rather than asserting a value)
	// makes the test independent of mtime precision: whatever the FS
	// stored, the next run sees mtime == last_indexed_at exactly — the
	// boundary the fix turns from "skip" into "re-index".
	writeFile(t, fpath, "package main\n\nfunc A() {}\n\nfunc B() {}\n")
	fi, err := os.Stat(fpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastIndexedAt(ctx, fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	after, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Chunks <= before.Chunks {
		t.Fatalf("equal-mtime edit was skipped by the mtime fast-path: "+
			"chunks %d → %d (want an increase from the added declaration)",
			before.Chunks, after.Chunks)
	}
}

// flakyEmbedder hash-embeds like fakeEmbedServer but fails any batch whose
// input contains failMarker (per-batch isolation) or, with failAll, every
// batch (total-outage). BatchSize is 1 so each chunk is its own batch, making
// "this one chunk's batch fails" deterministic regardless of chunk ordering.
type flakyEmbedder struct {
	failMarker string
	failAll    bool
}

func (flakyEmbedder) Health(context.Context) error { return nil }
func (flakyEmbedder) Endpoint() string             { return "flaky://test" }
func (flakyEmbedder) ModelName() string            { return "fake" }
func (flakyEmbedder) BatchSize() int               { return 1 }

func (f flakyEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	if f.failAll {
		return nil, fmt.Errorf("flaky: backend down")
	}
	for _, in := range inputs {
		if f.failMarker != "" && strings.Contains(in, f.failMarker) {
			return nil, fmt.Errorf("flaky: poisoned batch")
		}
	}
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		out[i] = hashVec(in, 16)
	}
	return out, nil
}

// openIndexerStore wires a temp project + store + ignore for the embed-pass
// tests and returns the project handle, open store, and ignore matcher.
func openIndexerStore(t *testing.T, projDir, cacheDir string) (*proj.Project, *store.Store, *ignore.Matcher) {
	t.Helper()
	ctx := context.Background()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ig, err := ignore.New(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	return p, st, ig
}

// TestIndexEmbedBatchIsolation: one bad embed batch is logged and skipped, the
// run still succeeds, the good files land, and a later healthy run backfills
// the skipped chunk. Regression for #438 (one bad file killed the whole pass).
func TestIndexEmbedBatchIsolation(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	writeFile(t, filepath.Join(projDir, "good_a.go"),
		"package main\n\n// Aaa does a.\nfunc Aaa() string { return \"aaa\" }\n")
	writeFile(t, filepath.Join(projDir, "good_b.go"),
		"package main\n\n// Bbb does b.\nfunc Bbb() string { return \"bbb\" }\n")
	// EmbedText stamps "// path: <file>" into every chunk, so a marker in the
	// filename poisons all of this file's batches (its chunks never embed),
	// regardless of how the file splits into chunks.
	const poisonFile = "POISONPILL_ccc.go"
	writeFile(t, filepath.Join(projDir, poisonFile),
		"package main\n\n// Ccc returns c.\nfunc Ccc() string { return \"ccc\" }\n")

	ctx := context.Background()
	p, st, ig := openIndexerStore(t, projDir, cacheDir)

	ix := New(p, st, flakyEmbedder{failMarker: "POISONPILL"}, ig, Options{})
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("Run with one poisoned batch must succeed (isolation): %v", err)
	}

	existing, err := st.ExistingSHAsBatch(ctx, []string{"good_a.go", "good_b.go", poisonFile})
	if err != nil {
		t.Fatal(err)
	}
	if len(existing["good_a.go"]) == 0 || len(existing["good_b.go"]) == 0 {
		t.Errorf("good files must be embedded despite the poisoned batch; got a=%d b=%d",
			len(existing["good_a.go"]), len(existing["good_b.go"]))
	}
	if n := len(existing[poisonFile]); n != 0 {
		t.Errorf("poisoned file's batches must leave no chunk rows; got %d", n)
	}

	// Recovery: a healthy embedder backfills the skipped chunks on the next run.
	ix2 := New(p, st, flakyEmbedder{}, ig, Options{})
	if err := ix2.Run(ctx); err != nil {
		t.Fatalf("recovery Run: %v", err)
	}
	recovered, err := st.ExistingSHAsBatch(ctx, []string{poisonFile})
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered[poisonFile]) == 0 {
		t.Error("previously-skipped chunks should be backfilled on the recovery run")
	}
}

// TestIndexEmbedAllBatchesFail: isolation must not mask a total outage as a
// run that silently embedded nothing — when every batch fails, Run fails loud.
func TestIndexEmbedAllBatchesFail(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeIndexAll(t, projDir)

	writeFile(t, filepath.Join(projDir, "a.go"),
		"package main\n\n// Aaa does a.\nfunc Aaa() string { return \"aaa\" }\n")
	writeFile(t, filepath.Join(projDir, "b.go"),
		"package main\n\n// Bbb does b.\nfunc Bbb() string { return \"bbb\" }\n")

	ctx := context.Background()
	p, st, ig := openIndexerStore(t, projDir, cacheDir)

	ix := New(p, st, flakyEmbedder{failAll: true}, ig, Options{})
	err := ix.Run(ctx)
	if err == nil {
		t.Fatal("Run must fail loud when every embed batch fails (total outage)")
	}
	if !strings.Contains(err.Error(), "all") {
		t.Errorf("error should report that all batches failed; got %v", err)
	}
}
