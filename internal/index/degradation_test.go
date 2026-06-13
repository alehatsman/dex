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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// Track 1 of epic #174 — index-time partial embed failure.
//
// Contract under test (internal/index/index.go:701 per-batch embed+upsert
// loop): when the embed backend fails MID-INDEX — succeeds for the first K
// batches, then errors on batch K+1 — the already-embedded batches are
// persisted, the run surfaces the embed error, and the NEXT run RESUMES,
// embedding only the not-yet-committed chunks (content-SHA skip) instead of
// re-embedding committed work or corrupting the index.

// hashEmbed deterministically maps a string to a dim-length vector. Same
// input always yields the same vector — so re-embedding a previously stored
// chunk would be detectable as a redundant call, and so a resumed run can be
// validated against the first full run.
func hashEmbed(s string, dim int) []float32 {
	out := make([]float32, dim)
	h := sha256.Sum256([]byte(s))
	for i := range dim {
		u := binary.LittleEndian.Uint32(h[(i*4)%len(h):])
		out[i] = float32(int32(u)) / float32(math.MaxInt32)
	}
	return out
}

// countingEmbedServer is an OpenAI-compatible /v1/embeddings stub that
// records how many input strings it has been asked to embed and can be
// flipped to "crash" after a number of HTTP requests. A crash hijacks and
// closes the TCP connection mid-flight, which surfaces in embed.Client as
// embed.ErrUnreachable — the realistic "embedding service crashed / timed
// out" failure mode the issue calls for (vs. a clean HTTP 500).
type countingEmbedServer struct {
	srv *httptest.Server
	dim int

	mu         sync.Mutex
	reqs       int      // total HTTP requests received
	inputCount int      // total input strings embedded across all requests
	inputs     []string // every input string the server was asked to embed

	failAfter atomic.Int64 // if >0, requests strictly after this index drop the connection
}

func newCountingEmbedServer(dim int) *countingEmbedServer {
	c := &countingEmbedServer{dim: dim}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	return c
}

func (c *countingEmbedServer) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Input []string `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	c.mu.Lock()
	c.reqs++
	reqNum := c.reqs
	c.inputCount += len(body.Input)
	c.inputs = append(c.inputs, body.Input...)
	c.mu.Unlock()

	// Crash mode: drop the connection so embed.Client sees a transport
	// error (ErrUnreachable), modelling a backend that died mid-index.
	if fa := c.failAfter.Load(); fa > 0 && int64(reqNum) > fa {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}

	type item struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	}
	out := struct {
		Data []item `json:"data"`
	}{}
	for i, s := range body.Input {
		out.Data = append(out.Data, item{Index: i, Embedding: hashEmbed(s, c.dim)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (c *countingEmbedServer) Close() { c.srv.Close() }

func (c *countingEmbedServer) stats() (reqs, inputCount int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs, c.inputCount
}

// writeDegradationFixture lays down nFiles tiny Go files (one function each)
// under dir and opts the project into indexing everything. Returns the
// project dir. Each file is distinct so it produces a distinct chunk.
func writeDegradationFixture(t *testing.T, nFiles int) string {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".dex")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Indexing is opt-in via .dex/config.yml index.include; without it the
	// matcher skips every file and the index is empty.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yml"),
		[]byte("index:\n  include: [\"*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range nFiles {
		src := fmt.Sprintf("package fixture\n\n// Fn%d does work number %d.\nfunc Fn%d() int { return %d }\n", i, i, i, i)
		p := filepath.Join(dir, fmt.Sprintf("file%02d.go", i))
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runIndex resolves projDir against cacheDir, indexes it against an embed
// backend at srvURL, and returns ix.Run's error. The project's DB lives at the
// proj-resolved p.DBPath (cacheDir/<hash>/index.db) — the same path chunkCount
// reads — so a fixture indexed here is observable there. Reusing the same
// (projDir, cacheDir) pair across calls hits the same store, which is exactly
// what the resume assertions rely on.
//
// The index batch loop (index.go:701) uses ix.Embed.Batch as its batch size and
// calls Embed once per batch with exactly that many texts; with len(inputs)==
// Batch, embed.Client issues exactly one HTTP request per index batch. So with
// batch==Embed.Batch the server's request counter maps 1:1 onto index batches —
// the basis for a deterministic "fail after K batches".
func runIndex(t *testing.T, ctx context.Context, projDir, cacheDir, srvURL string, batch int) error {
	t.Helper()
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
	ig, _ := ignore.New(p.Root)
	em := embed.New(srvURL, "fake", batch, 5*time.Second)
	ix := New(p, st, em, ig, Options{})
	return ix.Run(ctx)
}

// chunkCount resolves (projDir, cacheDir) to the same p.DBPath runIndex wrote
// and returns its committed chunk count.
func chunkCount(t *testing.T, projDir, cacheDir string) int {
	t.Helper()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats, err := st.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.Chunks
}

// TestIndexPartialEmbedFailureSurvivesAndResumes pins the core contract:
// a mid-index embed crash is isolated (#438) — earlier batches stay committed,
// the run still succeeds, and the next run embeds only the remaining chunks.
func TestIndexPartialEmbedFailureSurvivesAndResumes(t *testing.T) {
	const (
		dim    = 16
		nFiles = 12
		batch  = 2 // small batch -> many index batches to fail between
		failK  = 3 // succeed for 3 batches, crash on the 4th
	)
	ctx := context.Background()

	// --- Baseline: a fully healthy index over an identical fixture, to
	// learn the total chunk count without hardcoding chunks-per-file. ---
	baseDir := writeDegradationFixture(t, nFiles)
	baseCache := t.TempDir()
	baseSrv := newCountingEmbedServer(dim)
	defer baseSrv.Close()
	if err := runIndex(t, ctx, baseDir, baseCache, baseSrv.srv.URL, batch); err != nil {
		t.Fatalf("baseline index: %v", err)
	}
	total := chunkCount(t, baseDir, baseCache)
	if total < failK*batch+batch {
		// Need enough chunks that batch failK+1 exists AND chunks remain
		// after it, otherwise the resume assertion is vacuous.
		t.Fatalf("fixture too small: total=%d, need > %d", total, failK*batch+batch)
	}

	// --- Failing run: same fixture, embed server crashes after failK
	// batches. ---
	projDir := writeDegradationFixture(t, nFiles)
	cacheDir := t.TempDir()

	failSrv := newCountingEmbedServer(dim)
	defer failSrv.Close()
	failSrv.failAfter.Store(failK)

	err := runIndex(t, ctx, projDir, cacheDir, failSrv.srv.URL, batch)

	// (a) the run SUCCEEDS: per-batch isolation (#438) logs and skips the
	// crashed batches and continues, rather than aborting the whole pass. (A
	// *total* outage — every batch failing — still fails loud; that is covered
	// by TestIndexEmbedAllBatchesFail. Here failK batches succeed first.)
	if err != nil {
		t.Fatalf("partial embed failure must be isolated, not surfaced: %v", err)
	}

	// (b) the store contains exactly the chunks from the surviving batches:
	// the first failK batches committed, batch failK+1 (which crashed) and
	// everything after it did not. With Concurrency=1 the upsert for batch
	// failK happens before the embed call for batch failK+1, so survivors
	// are deterministic: failK*batch chunks.
	survived := chunkCount(t, projDir, cacheDir)
	wantSurvived := failK * batch
	if survived != wantSurvived {
		t.Fatalf("survived chunks=%d, want %d (first %d batches of %d)",
			survived, wantSurvived, failK, batch)
	}
	if survived == 0 {
		t.Fatal("mid-index failure wiped every batch — earlier batches must survive")
	}
	if survived >= total {
		t.Fatalf("survived=%d >= total=%d — failure did not actually interrupt indexing",
			survived, total)
	}

	// --- Resume run: healthy server, same store. Must embed ONLY the
	// not-yet-committed chunks (content-SHA skip), not redo committed work. ---
	healthySrv := newCountingEmbedServer(dim)
	defer healthySrv.Close()
	if err := runIndex(t, ctx, projDir, cacheDir, healthySrv.srv.URL, batch); err != nil {
		t.Fatalf("resume index: %v", err)
	}

	// (c) final chunk count reaches the full total — the index is complete
	// and uncorrupted.
	final := chunkCount(t, projDir, cacheDir)
	if final != total {
		t.Fatalf("after resume chunks=%d, want full total %d", final, total)
	}

	// (c) the resume server embedded EXACTLY the remaining chunks, proving
	// committed chunks were skipped via content-SHA rather than re-embedded.
	_, resumeInputs := healthySrv.stats()
	wantResumeInputs := total - survived
	if resumeInputs != wantResumeInputs {
		t.Fatalf("resume embedded %d inputs, want exactly %d (total %d - survived %d); "+
			"a different count means committed chunks were re-embedded or work was lost",
			resumeInputs, wantResumeInputs, total, survived)
	}
}

// TestIndexCtxCancelPreservesCommitted pins (d): a cancelled index run bails
// promptly with an error WITHOUT dropping the batches that already committed,
// and a later healthy run resumes to a complete, uncorrupted index.
//
// Note on the error type: the index bails via two mechanisms — the per-batch
// `ctx.Err()` check at the top of the loop (index.go:720), which surfaces
// context.Canceled, OR an embed call whose in-flight HTTP request is cancelled,
// which embed.Client flattens into embed.ErrUnreachable (it wraps the cause
// with %v, so context.Canceled is not in the chain). Which one wins is a race
// between the cancel landing and the loop reaching its next iteration, so this
// test does NOT assert the specific error type — only that the run bails with
// an error and preserves committed work. (The "outer-cancel is not masked as
// unreachable" guarantee is covered in the retrieval layer:
// internal/retrieve TestRerankTimeoutDoesNotMaskCallerCancel.)
func TestIndexCtxCancelPreservesCommitted(t *testing.T) {
	const (
		dim         = 16
		nFiles      = 12
		batch       = 2
		cancelAfter = 2 // let ~2 batches commit, then cancel
	)

	// Baseline total over an identical fixture.
	baseDir := writeDegradationFixture(t, nFiles)
	baseCache := t.TempDir()
	baseSrv := newCountingEmbedServer(dim)
	defer baseSrv.Close()
	if err := runIndex(t, context.Background(), baseDir, baseCache, baseSrv.srv.URL, batch); err != nil {
		t.Fatalf("baseline index: %v", err)
	}
	total := chunkCount(t, baseDir, baseCache)

	projDir := writeDegradationFixture(t, nFiles)
	cacheDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelSrv := newCountingEmbedServer(dim)
	defer cancelSrv.Close()

	// Trip cancel once `cancelAfter` batches have been served, so the run is
	// interrupted partway through.
	base := cancelSrv.srv.Config.Handler
	cancelSrv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base.ServeHTTP(w, r)
		if reqs, _ := cancelSrv.stats(); reqs >= cancelAfter {
			cancel()
		}
	})

	err := runIndex(t, ctx, projDir, cacheDir, cancelSrv.srv.URL, batch)
	if err == nil {
		t.Fatal("expected a cancelled run to surface an error, got nil")
	}

	// Committed batches survive the cancellation; not everything was indexed.
	survived := chunkCount(t, projDir, cacheDir)
	if survived == 0 {
		t.Fatal("cancellation wiped every committed batch — committed work must survive")
	}
	if survived >= total {
		t.Fatalf("survived=%d >= total=%d — cancel did not interrupt indexing", survived, total)
	}

	// A later healthy run completes the index without corruption.
	healthySrv := newCountingEmbedServer(dim)
	defer healthySrv.Close()
	if err := runIndex(t, context.Background(), projDir, cacheDir, healthySrv.srv.URL, batch); err != nil {
		t.Fatalf("resume after cancel: %v", err)
	}
	if final := chunkCount(t, projDir, cacheDir); final != total {
		t.Fatalf("after resume chunks=%d, want %d", final, total)
	}
}
