package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closedURL returns an http:// URL pointing at a port guaranteed to refuse
// connections (listen on :0, record addr, close, return URL). More reliable
// than a hardcoded :1 which may be occupied on some environments (e.g. WSL2).
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

type req struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

func ok(dim int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body req
		_ = json.NewDecoder(r.Body).Decode(&body)
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		out := struct {
			Data  []item `json:"data"`
			Model string `json:"model"`
		}{Model: body.Model}
		for i := range body.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + j)
			}
			out.Data = append(out.Data, item{Index: i, Embedding: v})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func TestEmbedRoundTrip(t *testing.T) {
	srv := httptest.NewServer(ok(8))
	defer srv.Close()
	c := New(srv.URL, "fake", 4, 5*time.Second)
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 8 {
			t.Errorf("vec[%d] dim=%d, want 8", i, len(v))
		}
	}
}

func TestEmbedBatchingHonored(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		ok(4).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := New(srv.URL, "fake", 3, 5*time.Second)
	if _, err := c.Embed(context.Background(), []string{"a", "b", "c", "d", "e", "f", "g"}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 batches for batch=3, len=7; got %d", got)
	}
}

func TestEmbedUnreachable(t *testing.T) {
	c := New(closedURL(t), "fake", 4, 200*time.Millisecond)
	c.MaxRetries = 0 // this test checks single-attempt error surfacing, not retry
	_, err := c.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("expected ErrUnreachable, got %v", err)
	}
}

func TestEmbedServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, "fake", 4, 2*time.Second)
	c.MaxRetries = 0 // retry behavior is covered separately; assert error surfacing
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected http 503 error, got %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("http://example/", "m", 0, 0)
	if c.Batch != 32 {
		t.Errorf("Batch default = %d, want 32", c.Batch)
	}
	if c.HTTP.Timeout != 60*time.Second {
		t.Errorf("Timeout default = %s, want 60s", c.HTTP.Timeout)
	}
	if strings.HasSuffix(c.BaseURL, "/") {
		t.Errorf("BaseURL should be trimmed: %q", c.BaseURL)
	}
	if c.Concurrency != 1 {
		t.Errorf("Concurrency default = %d, want 1 (sequential)", c.Concurrency)
	}
	if c.MaxRetries != 3 {
		t.Errorf("MaxRetries default = %d, want 3", c.MaxRetries)
	}
	if c.RetryBaseDelay <= 0 {
		t.Errorf("RetryBaseDelay default = %s, want > 0", c.RetryBaseDelay)
	}
}

// TestEmbedConcurrentDispatch verifies that NewWithConcurrency lets multiple
// batches actually fly in parallel. The handler blocks until it sees the
// configured number of simultaneous requests, then releases them — if
// dispatch were still sequential the test would hang and fail.
func TestEmbedConcurrentDispatch(t *testing.T) {
	const conc = 4
	var (
		inFlight    atomic.Int32
		peak        atomic.Int32
		totalCalls  atomic.Int32
		releaseGate = make(chan struct{})
		readyGate   = make(chan struct{}, conc)
	)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		totalCalls.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		select {
		case readyGate <- struct{}{}:
		default:
		}
		<-releaseGate
		ok(4).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := NewWithConcurrency(srv.URL, "fake", 1, conc, 5*time.Second)
	inputs := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	done := make(chan error, 1)
	var vecs [][]float32
	go func() {
		var err error
		vecs, err = c.Embed(context.Background(), inputs)
		done <- err
	}()

	deadline := time.After(2 * time.Second)
	for i := 0; i < conc; i++ {
		select {
		case <-readyGate:
		case <-deadline:
			t.Fatalf("only %d concurrent requests reached the server (want %d)", peak.Load(), conc)
		}
	}
	close(releaseGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got < conc {
		t.Errorf("peak in-flight = %d, want >= %d", got, conc)
	}
	if got := totalCalls.Load(); got != int32(len(inputs)) {
		t.Errorf("total calls = %d, want %d", got, len(inputs))
	}
	if len(vecs) != len(inputs) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(inputs))
	}
	for i := range inputs {
		if len(vecs[i]) != 4 {
			t.Errorf("vec[%d] dim=%d, want 4", i, len(vecs[i]))
		}
	}
}

func TestEmbedConcurrentError(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 2 {
			http.Error(w, "boom", 500)
			return
		}
		ok(4).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := NewWithConcurrency(srv.URL, "fake", 1, 4, 5*time.Second)
	c.MaxRetries = 0 // assert error propagation through the dispatcher, not retry
	_, err := c.Embed(context.Background(), []string{"a", "b", "c", "d"})
	if err == nil {
		t.Fatal("expected error from failing batch, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected http 500 error, got %v", err)
	}
}

// TestEmbedRetriesTransient guards #436: a transient 5xx must be retried with
// backoff rather than aborting the batch. The handler fails twice with 503
// then succeeds; Embed should return vectors after exactly 3 calls.
func TestEmbedRetriesTransient(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		ok(4).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c := New(srv.URL, "fake", 4, 5*time.Second)
	c.RetryBaseDelay = time.Millisecond // keep the test fast
	vecs, err := c.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 4 {
		t.Fatalf("unexpected result shape: %v", vecs)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("call count = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestEmbedRetryExhaustion guards #436: a persistent 5xx is retried MaxRetries
// times then surfaces the error (MaxRetries+1 total attempts).
func TestEmbedRetryExhaustion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, "fake", 4, 5*time.Second)
	c.MaxRetries = 2
	c.RetryBaseDelay = time.Millisecond
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected http 502 error after retries, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("call count = %d, want 3 (1 + 2 retries)", got)
	}
}

// TestEmbedNoRetryOn4xx guards #436: a 4xx is deterministic and must NOT be
// retried — one attempt only.
func TestEmbedNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad model", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(srv.URL, "fake", 4, 5*time.Second)
	c.RetryBaseDelay = time.Millisecond
	_, err := c.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected http 400 error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("call count = %d, want 1 (4xx must not retry)", got)
	}
}

// embedItem mirrors one element of an OpenAI-style embeddings response.
type embedItem struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// faultyEmbedServer returns dim-wide vectors for each input, then lets the
// test mutate the response items to inject the corruption under test.
func faultyEmbedServer(dim int, mutate func([]embedItem) []embedItem) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body req
		_ = json.NewDecoder(r.Body).Decode(&body)
		items := make([]embedItem, 0, len(body.Input))
		for i := range body.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + j + 1) // non-zero so "all-zero" is a deliberate fault
			}
			items = append(items, embedItem{Index: i, Embedding: v})
		}
		if mutate != nil {
			items = mutate(items)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Data  []embedItem `json:"data"`
			Model string      `json:"model"`
		}{Data: items, Model: body.Model})
	})
}

// TestEmbedRejectsCorruptVectors guards #435: malformed vectors must error out
// rather than be silently written to the index. EnsureEmbedModel only checks
// the model-name string, and the per-batch length check counts vectors without
// inspecting them, so without this validation a misbehaving server corrupts
// cosine search until a manual reindex.
func TestEmbedRejectsCorruptVectors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]embedItem) []embedItem
		want   string
	}{
		{"empty vector", func(it []embedItem) []embedItem { it[1].Embedding = []float32{}; return it }, "empty"},
		{"short vector", func(it []embedItem) []embedItem { it[1].Embedding = it[1].Embedding[:2]; return it }, "width"},
		{"all-zero vector", func(it []embedItem) []embedItem {
			for j := range it[1].Embedding {
				it[1].Embedding[j] = 0
			}
			return it
		}, "all-zero"},
		{"duplicate index", func(it []embedItem) []embedItem { it[2].Index = 0; return it }, "duplicate index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(faultyEmbedServer(8, tc.mutate))
			defer srv.Close()
			c := New(srv.URL, "fake", 8, 5*time.Second) // batch >= inputs: single batch
			_, err := c.Embed(context.Background(), []string{"a", "b", "c", "d"})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestEmbedRejectsCrossBatchWidthMismatch forces multiple concurrent batches
// (batchSize 1) where the server serves a wider vector for one input. No
// single batch is internally inconsistent, so only the final whole-result
// validation can catch it (#435).
func TestEmbedRejectsCrossBatchWidthMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body req
		_ = json.NewDecoder(r.Body).Decode(&body)
		dim := 8
		if len(body.Input) > 0 && strings.Contains(body.Input[0], "wide") {
			dim = 16
		}
		items := make([]embedItem, 0, len(body.Input))
		for i := range body.Input {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + j + 1)
			}
			items = append(items, embedItem{Index: i, Embedding: v})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Data  []embedItem `json:"data"`
			Model string      `json:"model"`
		}{Data: items, Model: body.Model})
	}))
	defer srv.Close()
	c := New(srv.URL, "fake", 1, 5*time.Second) // one batch per input → concurrent path
	_, err := c.Embed(context.Background(), []string{"a", "wide", "c"})
	if err == nil || !strings.Contains(err.Error(), "width") {
		t.Fatalf("expected cross-batch width-mismatch error, got %v", err)
	}
}
