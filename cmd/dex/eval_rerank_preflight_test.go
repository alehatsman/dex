package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// isolateRerankEnv clears the reranker env for the duration of a test so an
// ambient DEX_RERANK_URL on the dev box can't decide the outcome.
func isolateRerankEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"DEX_RERANK_URL", "DEX_DISABLE_RERANK", "DEX_RERANK_STYLE", "DEX_RERANK_MODEL", "DEX_RERANK_TIMEOUT"} {
		old, had := os.LookupEnv(k)
		k := k
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
		os.Unsetenv(k)
	}
}

// TestEvalRerankLive pins #864: the eval manifest's rerank_enabled must record
// REACHABILITY, not configuration. A reranker that is wired but does not answer
// degrades every query to the local rerank, and labelling those numbers
// "reranked" makes them silently comparable to a live cross-encoder baseline.
func TestEvalRerankLive(t *testing.T) {
	ctx := context.Background()

	t.Run("not configured -> false", func(t *testing.T) {
		isolateRerankEnv(t)
		if got := evalRerankLive(ctx, store.Options{}); got {
			t.Error("evalRerankLive = true with no reranker wired; want false")
		}
	})

	t.Run("reachable -> true", func(t *testing.T) {
		isolateRerankEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		os.Setenv("DEX_RERANK_URL", srv.URL)

		opts := storeOpts()
		if opts.Rerank == nil {
			t.Fatal("storeOpts did not wire a reranker for a live DEX_RERANK_URL")
		}
		if !evalRerankLive(ctx, opts) {
			t.Error("evalRerankLive = false against a healthy endpoint; want true")
		}
	})

	// The outage that actually bit: the vLLM reranker answering 404 on
	// /v1/models. ChatReranker.Health reports that as a plain error, not
	// rerank.ErrUnreachable — the preflight must treat any Health error as
	// not-reachable or this exact case slips through.
	t.Run("configured but 404 -> false", func(t *testing.T) {
		isolateRerankEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()
		os.Setenv("DEX_RERANK_URL", srv.URL)

		opts := storeOpts()
		if opts.Rerank == nil {
			t.Fatal("storeOpts did not wire a reranker for a set DEX_RERANK_URL")
		}
		if evalRerankLive(ctx, opts) {
			t.Error("evalRerankLive = true against a 404 endpoint; the run measures WITHOUT the cross-encoder")
		}
	})

	// #830's BM25 bench-gate baseline runs with DEX_DISABLE_RERANK=1 and a
	// URL still in the environment; rerank_enabled was already correctly false
	// there and must stay that way — without a probe of the disabled endpoint.
	t.Run("DEX_DISABLE_RERANK=1 -> false, no probe", func(t *testing.T) {
		isolateRerankEnv(t)
		probed := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probed = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		os.Setenv("DEX_RERANK_URL", srv.URL)
		os.Setenv("DEX_DISABLE_RERANK", "1")

		opts := storeOpts()
		if opts.Rerank != nil {
			t.Fatal("DEX_DISABLE_RERANK=1 still wired a reranker into store options")
		}
		if evalRerankLive(ctx, opts) {
			t.Error("evalRerankLive = true with reranking disabled; want false")
		}
		if probed {
			t.Error("probed a disabled reranker endpoint; the nil hook is answer enough")
		}
	})
}

// TestProbeRerankUnconfigured pins the "nothing wired" contract of the probe
// itself: no endpoint, no error, no network call.
func TestProbeRerankUnconfigured(t *testing.T) {
	isolateRerankEnv(t)
	endpoint, err := probeRerank(context.Background())
	if endpoint != "" || err != nil {
		t.Errorf("probeRerank = (%q, %v); want (\"\", nil) with no reranker configured", endpoint, err)
	}
}
