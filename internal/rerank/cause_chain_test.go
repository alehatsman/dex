package rerank_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/rerank"
)

// TestRerankPreservesCauseChain (#868) pins that a transport failure keeps its
// cause reachable through errors.Is.
//
// The Rerank path wrapped the transport error with %v, which stringifies it and
// severs the chain. Everything still looked right — the error message named the
// timeout, ErrUnreachable still matched, the fallback still fired — but
// context.DeadlineExceeded was no longer detectable, so the layer above could
// not tell its own expired deadline from an endpoint that never answered, and
// reported every timeout as an outage.
func TestRerankPreservesCauseChain(t *testing.T) {
	// A server that answers slower than the caller's deadline. It sleeps a
	// bounded amount rather than blocking on the request context, so Close()
	// never waits on an in-flight handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := rerank.New(srv.URL, "test-model", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Rerank(ctx, "query", []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected an error from a server that never answers")
	}
	if !errors.Is(err, rerank.ErrUnreachable) {
		t.Errorf("err does not match ErrUnreachable (%v) — the degradation path would stop firing", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err lost its cause: %v — errors.Is(err, context.DeadlineExceeded) is false, so a caller "+
			"cannot tell its own timeout from a dead endpoint (wrap with %%w, not %%v)", err)
	}
}
