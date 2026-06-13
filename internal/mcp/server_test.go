package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/index"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
)

func fakeEmbed(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i, s := range body.Input {
			out.Data = append(out.Data, item{Index: i, Embedding: hashVec(s, dim)})
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

// indexProject indexes projDir into cacheDir using srvURL and returns
// the resolved project root (mcp.Server expects absolute paths).
func indexProject(t *testing.T, projDir, cacheDir, srvURL string) string {
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
	defer st.Close()
	writeIndexAll(t, projDir)
	ig, _ := ignore.New(p.Root)
	em := embed.New(srvURL, "fake", 16, 5*time.Second)
	ix := index.New(p, st, em, ig, index.Options{})
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	return p.Root
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeIndexAll opts the temp project into indexing everything. Indexing
// is opt-in (.dex/config.yml index.include); without an include list
// the matcher skips every file, so an index built for these server tests
// would be empty. Mirrors the include = ["*"] escape used in the ignore
// tests.
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

func newServer(srvURL, cacheDir string) *Server {
	return &Server{
		EmbedClient: embed.New(srvURL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
	}
}

func TestSearchOk(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\nfunc Bye() {}\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.search(context.Background(), nil, SearchInput{
		Query:       "greeting function",
		ProjectRoot: root,
		K:           5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if len(out.Hits) == 0 {
		t.Fatal("expected at least one hit")
	}
}

func TestSearchNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// No index pass.
	s := newServer(srv.URL, cacheDir)
	_, out, _ := s.search(context.Background(), nil, SearchInput{
		Query:       "anything",
		ProjectRoot: projDir,
	})
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
	if out.Hint == "" {
		t.Error("expected a hint for no-index")
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	for _, q := range []string{"", "   ", "\t\n  "} {
		_, out, _ := s.search(context.Background(), nil, SearchInput{Query: q})
		if out.Status != "error" {
			t.Errorf("query=%q status=%q, want error", q, out.Status)
		}
	}
}

func TestSearchBadProjectRoot(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	_, out, _ := s.search(context.Background(), nil, SearchInput{
		Query:       "x",
		ProjectRoot: "/this/path/does/not/exist",
	})
	if out.Status != "error" {
		t.Errorf("status = %q, want error", out.Status)
	}
}

func TestSearchEmbeddingUnreachable(t *testing.T) {
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// Need an indexed project first; index against a reachable server,
	// then point the MCP server at a dead one for the actual query.
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	root := indexProject(t, projDir, cacheDir, srv.URL)
	writeFile(t, filepath.Join(projDir, "x.go"), "package main\n")

	s := &Server{
		EmbedClient: embed.New(closedURL(t), "fake", 16, 200*time.Millisecond),
		IndexDir:    cacheDir,
	}
	_, out, _ := s.search(context.Background(), nil, SearchInput{
		Query:       "x",
		ProjectRoot: root,
	})
	if out.Status != "embedding-service-unreachable" {
		t.Errorf("status = %q, want embedding-service-unreachable", out.Status)
	}
	if out.Endpoint == "" {
		t.Error("expected Endpoint to be populated on unreachable")
	}
}

func TestSearchKClamping(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	for i := range 40 {
		writeFile(t, filepath.Join(projDir, "f", "g.go"),
			"package main\nfunc F() {}\n") // overwrites — only 1 file needed
		_ = i
	}
	// 40 small Go files so we have enough chunks to test clamping.
	for i := range 40 {
		writeFile(t, filepath.Join(projDir, "f", "f"+itoa(i)+".go"),
			"package main\nfunc F"+itoa(i)+"() {}\n")
	}
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, _ := s.search(context.Background(), nil, SearchInput{
		Query: "any", ProjectRoot: root, K: 1000,
	})
	if len(out.Hits) > 30 {
		t.Errorf("got %d hits, want ≤30 (clamp)", len(out.Hits))
	}
	_, out, _ = s.search(context.Background(), nil, SearchInput{
		Query: "any", ProjectRoot: root, K: -1,
	})
	if len(out.Hits) == 0 || len(out.Hits) > 8 {
		t.Errorf("k=-1 → got %d hits, want default 8", len(out.Hits))
	}
}

func TestStatusReachable(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "a.go"), "package main\n")
	indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, _ := s.status(context.Background(), nil, StatusInput{})
	if !out.Reachable {
		t.Errorf("Reachable = false, want true (error: %s)", out.Error)
	}
	if out.Version == "" {
		t.Error("Version field empty")
	}
	if len(out.Projects) == 0 {
		t.Error("Projects empty after indexing")
	}
}

func TestStatusUnreachable(t *testing.T) {
	s := &Server{
		EmbedClient: embed.New(closedURL(t), "fake", 16, 200*time.Millisecond),
		IndexDir:    t.TempDir(),
	}
	_, out, _ := s.status(context.Background(), nil, StatusInput{})
	if out.Reachable {
		t.Error("Reachable = true on a dead endpoint")
	}
	if out.Error == "" {
		t.Error("expected Error to be populated on unreachable")
	}
}

func TestSearchDefaultsToCwd(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "g.go"),
		"package main\nfunc G() {}\n")
	indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	// Chdir into projDir; an empty ProjectRoot should resolve there.
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	_ = os.Chdir(projDir)

	_, out, _ := s.search(context.Background(), nil, SearchInput{Query: "G"})
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok (project=%q)", out.Status, out.Project)
	}
}

func TestBuildSummarizeSystem(t *testing.T) {
	base := buildSummarizeSystem("")
	for _, want := range []string{
		"file summarizer",  // file-kind agnostic, not "code summarizer"
		"Makefiles",        // hint that non-code files have their own framing
		"top-level keys",   // config files
		"section headings", // docs
	} {
		if !strings.Contains(base, want) {
			t.Errorf("base prompt missing %q", want)
		}
	}
	if strings.Contains(base, "Focus specifically on") {
		t.Errorf("empty focus should not inject a focus clause; got: %s", base)
	}

	withFocus := buildSummarizeSystem("  public API surface  ")
	if !strings.Contains(withFocus, "Focus specifically on: public API surface.") {
		t.Errorf("focus clause missing or untrimmed; got: %s", withFocus)
	}
}

// stubReranker returns the docs in input order with descending
// scores; enough to drive a non-zero RerankScore on every Hit.
type stubReranker struct{}

func (stubReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Score, error) {
	out := make([]rerank.Score, len(docs))
	for i := range docs {
		out[i] = rerank.Score{Index: i, Score: 1.0 - float32(i)*0.1}
	}
	return out, nil
}

func TestSearchPopulatesRerankScore(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// 8 chunks so the fused pool exceeds k=3 and the rerank stage fires.
	for i := range 8 {
		writeFile(t, filepath.Join(projDir, "f"+itoa(i)+".go"),
			"package main\nfunc F"+itoa(i)+"() {}\n")
	}
	root := indexProject(t, projDir, cacheDir, srv.URL)

	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		StoreOpts:   store.Options{RerankOptions: store.RerankOptions{Reranker: stubReranker{}}},
	}
	_, out, err := s.search(context.Background(), nil, SearchInput{
		Query:       "function",
		ProjectRoot: root,
		K:           3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if len(out.Hits) == 0 {
		t.Fatal("expected hits")
	}
	for i, h := range out.Hits {
		if h.RerankScore <= 0 {
			t.Errorf("hit[%d] %q: RerankScore = %v, want > 0", i, h.Path, h.RerankScore)
		}
	}
}

func TestStatusReportsRerankEndpoint(t *testing.T) {
	embedSrv := fakeEmbed(t, 16)
	defer embedSrv.Close()

	rerankSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-reranker",
			"results": []map[string]any{{"index": 0, "relevance_score": 0.99}},
		})
	}))
	defer rerankSrv.Close()

	s := &Server{
		EmbedClient:  embed.New(embedSrv.URL, "fake-embed", 16, 5*time.Second),
		RerankClient: rerank.New(rerankSrv.URL, "fake-reranker", 5*time.Second),
		IndexDir:     t.TempDir(),
	}
	_, out, _ := s.status(context.Background(), nil, StatusInput{})
	if out.RerankEndpoint != rerankSrv.URL {
		t.Errorf("RerankEndpoint = %q, want %q", out.RerankEndpoint, rerankSrv.URL)
	}
	if out.RerankModel != "fake-reranker" {
		t.Errorf("RerankModel = %q, want fake-reranker", out.RerankModel)
	}
	if !out.RerankReachable {
		t.Error("RerankReachable = false, want true (fake server is up)")
	}
}

func TestStatusOmitsRerankWhenUnwired(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	_, out, _ := s.status(context.Background(), nil, StatusInput{})
	if out.RerankEndpoint != "" || out.RerankModel != "" || out.RerankReachable {
		t.Errorf("rerank fields should be zero when RerankClient is nil; got endpoint=%q model=%q reachable=%v",
			out.RerankEndpoint, out.RerankModel, out.RerankReachable)
	}
}

// fakeChat returns a test server that always responds with the given body
// as the assistant completion content.
func fakeChat(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": body}, "finish_reason": "stop"},
			},
			"model": "fake-chat",
		})
	}))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func TestFindSymbolNotFoundSurfaceCandidates(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"),
		"package main\n\nfunc Indexer() {}\nfunc IndexableExt() {}\nfunc cmdIndex() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.findSymbol(context.Background(), nil, FindSymbolInput{
		Name:        "Index", // no exact match, but substring of several
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "not-found" {
		t.Errorf("status=%q, want not-found", out.Status)
	}
	if !strings.Contains(out.Hint, "Did you mean") {
		t.Errorf("hint should propose near-misses; got %q", out.Hint)
	}
	// At least one real candidate should appear by name.
	if !strings.Contains(out.Hint, "Indexer") && !strings.Contains(out.Hint, "IndexableExt") && !strings.Contains(out.Hint, "cmdIndex") {
		t.Errorf("hint should name a real candidate; got %q", out.Hint)
	}
}

// ─── auto-watcher (lazy per-project watcher spawn) ────────────────────────

// countWatchers walks the (test-only) sync.Map of live watcher
// entries. The map is keyed by Project.ID; the value is presence-only.
func (s *Server) countWatchers() int {
	n := 0
	s.watchers.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestEnsureWatcherNoopWithoutRunCtx verifies that a Server built for
// a one-shot CLI invocation (no runCtx) never spawns a goroutine.
// This is the safety net that keeps `dex ask` and similar from leaking
// background watchers.
func TestEnsureWatcherNoopWithoutRunCtx(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS", filepath.Dir(projDir))
	writeFile(t, filepath.Join(projDir, "a.go"), "package main\n")

	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		AutoWatch:   AutoWatchConfig{Enabled: true},
	}
	// runCtx intentionally unset.
	p, _ := proj.Resolve(projDir, cacheDir)
	s.ensureWatcher(p)
	if got := s.countWatchers(); got != 0 {
		t.Errorf("ensureWatcher must be a no-op without runCtx; got %d watchers", got)
	}
}

// TestEnsureWatcherNoopWhenDisabled verifies that explicitly disabling
// AutoWatch keeps the server idle even in stdio mode.
func TestEnsureWatcherNoopWhenDisabled(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS", filepath.Dir(projDir))
	writeFile(t, filepath.Join(projDir, "a.go"), "package main\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		AutoWatch:   AutoWatchConfig{Enabled: false}, // explicitly off
	}
	s.runCtx = ctx
	p, _ := proj.Resolve(projDir, cacheDir)
	s.ensureWatcher(p)
	if got := s.countWatchers(); got != 0 {
		t.Errorf("ensureWatcher must be a no-op when disabled; got %d watchers", got)
	}
}

// TestEnsureWatcherSpawnsOncePerProject verifies the single-flight
// behaviour: many concurrent or repeated ensureWatcher calls for the
// same project result in exactly one Watcher goroutine. Cancelling
// runCtx unblocks RunStdio's deferred Wait so a real server can shut
// down cleanly.
func TestEnsureWatcherSpawnsOncePerProject(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS", filepath.Dir(projDir))
	writeFile(t, filepath.Join(projDir, "a.go"), "package main\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		AutoWatch: AutoWatchConfig{
			Enabled:  true,
			Debounce: 10 * time.Millisecond,
		},
	}
	s.runCtx = ctx
	p, _ := proj.Resolve(projDir, cacheDir)

	// Hammer ensureWatcher concurrently — only one goroutine should
	// actually start its Watcher.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ensureWatcher(p)
		}()
	}
	wg.Wait()

	// Wait for the watcher to actually start its loop before checking
	// the map — the goroutine inserts itself before Run() is reached.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.countWatchers() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.countWatchers(); got != 1 {
		t.Fatalf("expected exactly 1 watcher entry; got %d", got)
	}

	// Cancel runCtx; the watcher goroutine must exit and the WaitGroup
	// must drain promptly.
	cancel()
	done := make(chan struct{})
	go func() {
		s.watcherWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher goroutine never exited after ctx cancel")
	}
	if got := s.countWatchers(); got != 0 {
		t.Errorf("watcher should clean up its map entry on exit; got %d remaining", got)
	}
}

// TestEnsureWatcherSpawnedByResolveProject verifies the wiring chain:
// any tool that resolves a project triggers ensureWatcher. Uses the
// SearchInput path which calls resolveProject early.
func TestEnsureWatcherSpawnedByResolveProject(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS", filepath.Dir(projDir))
	writeFile(t, filepath.Join(projDir, "x.go"), "package main\n")
	indexProject(t, projDir, cacheDir, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		AutoWatch: AutoWatchConfig{
			Enabled:  true,
			Debounce: 10 * time.Millisecond,
		},
	}
	s.runCtx = ctx

	// Drive a search; resolveProject inside the handler should spawn.
	_, _, _ = s.search(ctx, nil, SearchInput{Query: "main", ProjectRoot: projDir})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.countWatchers() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.countWatchers(); got < 1 {
		t.Fatalf("expected resolveProject to spawn a watcher; got %d", got)
	}

	cancel()
	done := make(chan struct{})
	go func() { s.watcherWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never exited after ctx cancel")
	}
}

// TestStartEagerWatchersSpawnsForRegistry verifies serve's boot-time
// eager-watch spawns one watcher per registry project up-front — without
// waiting for a query — and skips a root that fails to resolve rather
// than aborting the whole sweep.
func TestStartEagerWatchersSpawnsForRegistry(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projA := t.TempDir()
	projB := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS",
		filepath.Dir(projA)+string(filepath.ListSeparator)+filepath.Dir(projB))
	writeFile(t, filepath.Join(projA, "a.go"), "package main\n")
	writeFile(t, filepath.Join(projB, "b.go"), "package main\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		EmbedClient: embed.New(srv.URL, "fake", 16, 5*time.Second),
		IndexDir:    cacheDir,
		AutoWatch: AutoWatchConfig{
			Enabled:  true,
			Debounce: 10 * time.Millisecond,
		},
	}
	s.runCtx = ctx

	registry, err := BuildProjectRegistry([]string{projA, projB})
	if err != nil {
		t.Fatalf("BuildProjectRegistry: %v", err)
	}
	// A root that doesn't resolve must be warned-and-skipped, not fatal.
	registry["deadbeef"] = filepath.Join(cacheDir, "does-not-exist")

	s.startEagerWatchers(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.countWatchers() == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.countWatchers(); got != 2 {
		t.Fatalf("expected 2 eager watchers (bad root skipped); got %d", got)
	}

	cancel()
	done := make(chan struct{})
	go func() { s.watcherWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchers never exited after ctx cancel")
	}
}

// TestStartEagerWatchersNoopWhenDisabled verifies the AutoWatch guard:
// with autowatch off, the boot-time sweep spawns nothing.
func TestStartEagerWatchersNoopWhenDisabled(t *testing.T) {
	cacheDir := t.TempDir()
	projA := t.TempDir()
	t.Setenv("DEX_ALLOW_PATHS", filepath.Dir(projA))
	writeFile(t, filepath.Join(projA, "a.go"), "package main\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		IndexDir:  cacheDir,
		AutoWatch: AutoWatchConfig{Enabled: false},
	}
	s.runCtx = ctx

	registry, err := BuildProjectRegistry([]string{projA})
	if err != nil {
		t.Fatalf("BuildProjectRegistry: %v", err)
	}
	s.startEagerWatchers(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := s.countWatchers(); got != 0 {
		t.Errorf("startEagerWatchers must be a no-op when AutoWatch disabled; got %d", got)
	}
}

func TestParseLinesRange(t *testing.T) {
	tests := []struct {
		in        string
		wantStart int
		wantEnd   int
		wantOK    bool
	}{
		{"10-40", 10, 40, true},
		{"1-1", 1, 1, true},
		{"1-100", 1, 100, true},
		{"", 0, 0, false},
		{"10", 0, 0, false},
		{"40-10", 0, 0, false}, // end < start
		{"0-10", 0, 0, false},  // start < 1
		{"abc-10", 0, 0, false},
		{"10-abc", 0, 0, false},
		{"-10", 0, 0, false},
	}
	for _, tc := range tests {
		s, e, ok := parseLinesRange(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseLinesRange(%q): ok=%v want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && (s != tc.wantStart || e != tc.wantEnd) {
			t.Errorf("parseLinesRange(%q): got %d-%d want %d-%d", tc.in, s, e, tc.wantStart, tc.wantEnd)
		}
	}
}

func TestSummarizeLinesMode(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	src := "line1\nline2\nline3\nline4\nline5\n"
	writeFile(t, filepath.Join(projDir, "f.txt"), src)

	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: cacheDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "f.txt",
		ProjectRoot: projDir,
		Mode:        "lines:2-4",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%q", out.Status, out.Hint)
	}
	if out.Model != "" || out.Endpoint != "" {
		t.Error("lines mode must not touch chat client fields")
	}
	for _, want := range []string{"line2", "line3", "line4"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("content missing %q; got: %s", want, out.Content)
		}
	}
	if strings.Contains(out.Content, "line1") || strings.Contains(out.Content, "line5") {
		t.Errorf("content leaked out-of-range lines; got: %s", out.Content)
	}
}

func TestSummarizeLinesEtagUnchanged(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "f.txt"), "line1\nline2\nline3\n")

	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: cacheDir}
	in := SummarizeInput{Path: "f.txt", ProjectRoot: projDir, Mode: "lines:1-3"}

	// First call: no etag — must return content.
	_, out1, err := s.summarize(context.Background(), nil, in)
	if err != nil || out1.Status != "ok" {
		t.Fatalf("first call: status=%q err=%v", out1.Status, err)
	}
	if out1.Etag == "" {
		t.Fatal("first call returned no etag")
	}

	// Second call with matching etag: must return status=unchanged.
	in.Etag = out1.Etag
	_, out2, err := s.summarize(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if out2.Status != "unchanged" {
		t.Errorf("status = %q, want unchanged (etag cache miss on stdio transport)", out2.Status)
	}
}

func TestSummarizeLinesModeInvalidRange(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "f.txt"), "hello\n")

	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}

	s := &Server{IndexDir: cacheDir}
	_, out, _ := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "f.txt",
		ProjectRoot: projDir,
		Mode:        "lines:bad",
	})
	if out.Status != "error" {
		t.Errorf("expected error status for bad range, got %q", out.Status)
	}
}

func TestSummarizeSignaturesModeNoIndex(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "f.go"), "package main\nfunc F() {}\n")

	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	// Open store so migration runs, but don't index — zero symbols.
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	st.Close()

	s := &Server{IndexDir: cacheDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "f.go",
		ProjectRoot: projDir,
		Mode:        "signatures",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%q", out.Status, out.Hint)
	}
	if out.Hint == "" {
		t.Error("expected a hint when no symbols indexed")
	}
}

func TestSummarizeMapModeNoIndex(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "f.go"), "package main\nfunc F() {}\n")

	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	st.Close()

	s := &Server{IndexDir: cacheDir}
	_, out, err := s.summarize(context.Background(), nil, SummarizeInput{
		Path:        "f.go",
		ProjectRoot: projDir,
		Mode:        "map",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%q", out.Status, out.Hint)
	}
	if out.Hint == "" {
		t.Error("expected a hint when no data indexed")
	}
	if out.Model != "" || out.Endpoint != "" {
		t.Error("map mode must not touch chat client fields")
	}
}

func TestFormatMap(t *testing.T) {
	syms := []store.GraphSymbol{
		{Name: "Server", QualifiedName: "mcp.Server", Kind: "struct", FilePath: "server.go", StartLine: 10, EndLine: 50},
		{Name: "unexported", QualifiedName: "mcp.unexported", Kind: "struct", FilePath: "server.go", StartLine: 55, EndLine: 60},
		{Name: "Run", QualifiedName: "mcp.Server.Run", Kind: "method", FilePath: "server.go", StartLine: 100, EndLine: 120},
	}
	imports := []string{"context", "fmt", "os"}
	got := formatMap("server.go", syms, imports)
	for _, want := range []string{
		"FILE: server.go",
		"IMPORTS:",
		"  context",
		"  fmt",
		"  os",
		"EXPORTS (2):",
		"struct mcp.Server",
		"method mcp.Server.Run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatMap missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unexported") {
		t.Errorf("formatMap leaked unexported symbol; got:\n%s", got)
	}
}

func TestFormatSignatures(t *testing.T) {
	src := []byte("package main\n\nfunc Foo() {\n\treturn\n}\n\nfunc Bar(x int) int {\n\treturn x\n}\n")
	syms := []store.GraphSymbol{
		{Name: "Foo", QualifiedName: "Foo", Kind: "function", FilePath: "f.go", StartLine: 3, EndLine: 5},
		{Name: "Bar", QualifiedName: "Bar", Kind: "function", FilePath: "f.go", StartLine: 7, EndLine: 9},
	}
	got := formatSignatures(src, syms, "f.go", nil)
	for _, want := range []string{
		"f.go",
		"(2 symbols)",
		"⊛ Foo (lines 3-5)",
		"func Foo()",
		"⊛ Bar (lines 7-9)",
		"func Bar(x int) int {",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatSignatures output missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestLangToExtensions(t *testing.T) {
	tests := []struct {
		langs []string
		want  []string
	}{
		{[]string{"go"}, []string{"go"}},
		{[]string{"typescript"}, []string{"ts", "tsx"}},
		{[]string{"Go", "TypeScript"}, []string{"go", "ts", "tsx"}},
		{[]string{".rs"}, []string{"rs"}},
		{[]string{"rs"}, []string{"rs"}},
		{[]string{"typescript", "javascript"}, []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"}},
		// dedup: typescript appears twice
		{[]string{"typescript", "typescript"}, []string{"ts", "tsx"}},
		{nil, nil},
	}
	for _, tt := range tests {
		got := langToExtensions(tt.langs)
		if len(got) != len(tt.want) {
			t.Errorf("langToExtensions(%v) = %v, want %v", tt.langs, got, tt.want)
			continue
		}
		for i, g := range got {
			if g != tt.want[i] {
				t.Errorf("langToExtensions(%v)[%d] = %q, want %q", tt.langs, i, g, tt.want[i])
			}
		}
	}
}

func TestFilterHits(t *testing.T) {
	makeHits := func(paths ...string) []store.Hit {
		hits := make([]store.Hit, len(paths))
		for i, p := range paths {
			hits[i].Path = p
		}
		return hits
	}
	paths := func(hits []store.Hit) []string {
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.Path
		}
		return out
	}

	// No filter — just trim.
	got := filterHits(makeHits("a.go", "b.go", "c.go"), nil, "", 2)
	if p := paths(got); len(p) != 2 || p[0] != "a.go" {
		t.Errorf("trim: got %v", p)
	}

	// Language filter: keep only .go
	got = filterHits(makeHits("a.go", "b.ts", "c.go", "d.py"), []string{"go"}, "", 10)
	if p := paths(got); len(p) != 2 || p[0] != "a.go" || p[1] != "c.go" {
		t.Errorf("lang filter go: got %v", p)
	}

	// Path glob filter.
	got = filterHits(makeHits("internal/mcp/server.go", "cmd/dex/main.go", "internal/store/store.go"), nil, "internal/**", 10)
	if p := paths(got); len(p) != 2 || p[0] != "internal/mcp/server.go" {
		t.Errorf("path glob: got %v", p)
	}

	// Combined: lang + glob.
	got = filterHits(makeHits("internal/mcp/server.go", "internal/mcp/server_test.go", "cmd/dex/main.go"), []string{"go"}, "internal/mcp/**", 10)
	if p := paths(got); len(p) != 2 {
		t.Errorf("lang+glob: got %v", p)
	}

	// Limit respected after filtering.
	got = filterHits(makeHits("a.go", "b.go", "c.go", "d.go"), []string{"go"}, "", 2)
	if p := paths(got); len(p) != 2 {
		t.Errorf("limit after filter: got %v", p)
	}
}

func TestFindRelatedOk(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	writeFile(t, filepath.Join(projDir, "bye.go"),
		"package main\n\nfunc Bye() string { return \"bye\" }\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.findRelated(context.Background(), nil, FindRelatedInput{
		FilePath:    "greet.go",
		Line:        3,
		ProjectRoot: root,
		K:           5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Source == nil {
		t.Fatal("expected Source chunk to be populated")
	}
	if out.Source.Path != "greet.go" {
		t.Errorf("source path = %q, want greet.go", out.Source.Path)
	}
	// Source chunk must not appear in results.
	for _, h := range out.Hits {
		if h.Path == out.Source.Path && h.StartLine == out.Source.StartLine {
			t.Errorf("source chunk appeared in hits: %+v", h)
		}
	}
}

func TestFindRelatedNotFound(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\n")

	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.findRelated(context.Background(), nil, FindRelatedInput{
		FilePath:    "main.go",
		Line:        9999,
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "not-found" {
		t.Errorf("status = %q, want not-found", out.Status)
	}
}

func TestFindRelatedEmptyFilePath(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	_, out, _ := s.findRelated(context.Background(), nil, FindRelatedInput{Line: 1})
	if out.Status != "error" {
		t.Errorf("status = %q, want error", out.Status)
	}
}

func TestSearchTreeFilePathReturnsError(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\nfunc main() {}\n")
	writeFile(t, filepath.Join(projDir, "util.go"), "package main\nfunc Util() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	_, out, err := s.searchTree(context.Background(), nil, SearchTreeInput{
		ProjectRoot: root,
		Path:        "main.go", // file, not directory
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want error; hint = %q", out.Status, out.Hint)
	}
	if !strings.Contains(out.Hint, "file, not a directory") {
		t.Fatalf("hint = %q, want message about file not directory", out.Hint)
	}
}

func TestApplyMultiScaleFilterFallsBackOnNoOverlap(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	// Five files so multi-scale index has material to produce >= 3 candidates.
	for _, name := range []string{"alpha.go", "beta.go", "gamma.go", "delta.go", "epsilon.go"} {
		writeFile(t, filepath.Join(projDir, name), "package main\n// "+name+" handles authentication\nfunc Handle() {}\n")
	}
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)

	p, hint := s.resolveProject(root)
	if hint != "" {
		t.Fatalf("resolveProject: %s", hint)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// A hit whose path is not in the indexed project; FilterByPaths would return empty.
	fakeHit := store.Hit{Path: "virtual/phantom/missing.go"}
	result := s.applyMultiScaleFilter(context.Background(), st, p.DBPath, "authentication", []store.Hit{fakeHit})

	// Without the fix, FilterByPaths would have silently dropped all hits.
	// With the fix we fall back to the original hits.
	if len(result) == 0 {
		t.Error("applyMultiScaleFilter: dropped all hits when candidate paths had no overlap — expected fallback to original hits")
	}
}
