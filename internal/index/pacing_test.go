package index

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/lock"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// closedURL returns an http:// URL pointing at a port guaranteed to refuse
// connections (listen :0, record addr, close). Safer than hardcoded :1.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("closedURL: listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

// countingChat is a fake chat endpoint that records how many requests
// it served — used to prove the drainer did NOT generate when it should
// have yielded/skipped.
func countingChat(t *testing.T) (*chat.Client, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"model":"fake"}`))
	}))
	t.Cleanup(srv.Close)
	return chat.New(srv.URL, "fake", 5*time.Second), &n
}

func testIndexer(t *testing.T, opt Options) (*Indexer, *proj.Project) {
	t.Helper()
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ig, _ := ignore.New(p.Root)
	em := embed.New(closedURL(t), "fake", 8, time.Second)
	return New(p, st, em, ig, opt), p
}

// TestPrunedDirsHandoff verifies the in-memory hand-off that lets the
// deletion-aware cascade (dex #234) see dirs whose file_summary rows Run()
// pruned: addPrunedDirs accumulates + dedupes, takePrunedDirs drains.
func TestPrunedDirsHandoff(t *testing.T) {
	ix, _ := testIndexer(t, Options{})

	if got := ix.takePrunedDirs(); got != nil {
		t.Fatalf("empty indexer takePrunedDirs=%v, want nil", got)
	}
	ix.addPrunedDirs(nil) // no-op, must not panic or allocate state
	if got := ix.takePrunedDirs(); got != nil {
		t.Fatalf("after addPrunedDirs(nil) takePrunedDirs=%v, want nil", got)
	}

	ix.addPrunedDirs([]string{"pkg/a", "pkg/b"})
	ix.addPrunedDirs([]string{"pkg/b", "pkg/c"}) // pkg/b duplicate

	got := ix.takePrunedDirs()
	set := make(map[string]bool, len(got))
	for _, d := range got {
		set[d] = true
	}
	if len(got) != 3 || !set["pkg/a"] || !set["pkg/b"] || !set["pkg/c"] {
		t.Errorf("takePrunedDirs=%v, want deduped {pkg/a,pkg/b,pkg/c}", got)
	}
	// Drained: a second take returns nil.
	if got := ix.takePrunedDirs(); got != nil {
		t.Errorf("second takePrunedDirs=%v, want nil (drained)", got)
	}
}

func TestForegroundBusy(t *testing.T) {
	ix, p := testIndexer(t, Options{YieldWindow: time.Minute})

	if ix.foregroundBusy() {
		t.Fatal("no activity marker yet → should not be busy")
	}
	if err := p.MarkActivity(); err != nil {
		t.Fatal(err)
	}
	if !ix.foregroundBusy() {
		t.Fatal("just marked activity → should be busy within the window")
	}

	// With the feature off (YieldWindow=0), never busy even right after a mark.
	ix.Options.YieldWindow = 0
	if ix.foregroundBusy() {
		t.Fatal("YieldWindow=0 → feature off, must never report busy")
	}
}

func TestBatchForPace(t *testing.T) {
	ix, _ := testIndexer(t, Options{})
	if got := ix.batchForPace(); got != 0 {
		t.Errorf("pace=0 → batchForPace=%d, want 0 (unbounded)", got)
	}
	ix.Options.SummaryPace = time.Second
	if got := ix.batchForPace(); got != 10 {
		t.Errorf("pace>0 → batchForPace=%d, want 10 (bounded)", got)
	}
}

func TestIdleDrainerSkipsWhenDrainLockHeld(t *testing.T) {
	cc, calls := countingChat(t)
	ix, p := testIndexer(t, Options{Chat: cc})
	drain := ix.IdleSummaryDrainer(10)
	if drain == nil {
		t.Fatal("drainer should be non-nil when Chat is set")
	}

	// Simulate another process holding the per-project drain lock.
	held, err := lock.Acquire(p.DrainLockPath, lock.Holder{Command: "other"})
	if err != nil {
		t.Fatalf("acquire drain lock: %v", err)
	}

	done, err := drain(context.Background())
	if err != nil {
		t.Fatalf("drain returned err: %v", err)
	}
	if done {
		t.Error("lock held → drainer should re-arm (done=false), not stop")
	}
	if calls.Load() != 0 {
		t.Errorf("lock held → drainer must not generate; chat called %d times", calls.Load())
	}

	// Once the lock frees, the drainer proceeds (empty queue → done=true).
	_ = held.Release()
	done, err = drain(context.Background())
	if err != nil {
		t.Fatalf("drain after release: %v", err)
	}
	if !done {
		t.Error("lock free + empty queue → drainer should report done=true")
	}
}

func TestIdleDrainerYieldsToForeground(t *testing.T) {
	cc, calls := countingChat(t)
	ix, p := testIndexer(t, Options{Chat: cc, YieldWindow: time.Minute})
	drain := ix.IdleSummaryDrainer(10)

	if err := p.MarkActivity(); err != nil {
		t.Fatal(err)
	}
	done, err := drain(context.Background())
	if err != nil {
		t.Fatalf("drain returned err: %v", err)
	}
	if done {
		t.Error("recent foreground activity → drainer should re-arm (done=false)")
	}
	if calls.Load() != 0 {
		t.Errorf("should yield to foreground; chat called %d times", calls.Load())
	}
}
