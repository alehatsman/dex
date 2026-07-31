package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/backendhttp"
	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
)

// TestClassifyDeep pins the readiness classification matrix, including the
// embed-is-critical / chat-rerank-degrade split and the cold-timeout carve-out.
func TestClassifyDeep(t *testing.T) {
	embedP := endpointProbe{name: "embed", url: "http://e", model: "m-embed"}
	chatP := endpointProbe{name: "chat", url: "http://c", model: "m-chat"}

	t.Run("usable", func(t *testing.T) {
		c := classifyDeep(embedP, nil)
		if c.status != docOK {
			t.Fatalf("status=%v, want docOK", c.status)
		}
		if !strings.Contains(c.detail, "usable") {
			t.Errorf("detail=%q, want it to say usable", c.detail)
		}
	})

	t.Run("cold timeout is a non-critical warning", func(t *testing.T) {
		c := classifyDeep(embedP, context.DeadlineExceeded)
		if c.status != docWarn || c.critical {
			t.Fatalf("status=%v critical=%v, want docWarn non-critical", c.status, c.critical)
		}
		if !strings.Contains(c.detail, "cold") {
			t.Errorf("detail=%q, want a cold-load hint", c.detail)
		}
	})

	t.Run("embed unreachable is critical", func(t *testing.T) {
		c := classifyDeep(embedP, fmt.Errorf("%w: dial tcp", embed.ErrUnreachable))
		if c.status != docFail || !c.critical {
			t.Fatalf("status=%v critical=%v, want docFail critical", c.status, c.critical)
		}
		if !strings.Contains(c.detail, "UNREACHABLE") {
			t.Errorf("detail=%q, want UNREACHABLE", c.detail)
		}
	})

	t.Run("chat unreachable degrades (warn)", func(t *testing.T) {
		c := classifyDeep(chatP, fmt.Errorf("%w: dial tcp", chat.ErrUnreachable))
		if c.status != docWarn || c.critical {
			t.Fatalf("status=%v critical=%v, want docWarn non-critical", c.status, c.critical)
		}
	})

	t.Run("overloaded (429) is reachable-but-busy, not UNREACHABLE", func(t *testing.T) {
		rerankP := endpointProbe{name: "rerank", url: "http://r", model: "m-rerank"}
		// The rerank client wraps 429/5xx as ErrUnreachable but keeps the
		// "http <code>:" marker — deep mode must read the code, not the sentinel.
		c := classifyDeep(rerankP, fmt.Errorf("%w: %w", rerank.ErrUnreachable, &backendhttp.StatusError{Code: 429, Body: "model overloaded"}))
		if c.status != docWarn || c.critical {
			t.Fatalf("status=%v critical=%v, want docWarn non-critical", c.status, c.critical)
		}
		if strings.Contains(c.detail, "UNREACHABLE") || !strings.Contains(c.detail, "overloaded") {
			t.Errorf("detail=%q, want an overloaded/busy message, not UNREACHABLE", c.detail)
		}
	})

	t.Run("model not served -> not ready with targeted hint", func(t *testing.T) {
		c := classifyDeep(embedP, fmt.Errorf("embed: %w", &backendhttp.StatusError{Code: 404, Body: "model not found"}))
		if c.status != docFail || !c.critical {
			t.Fatalf("status=%v critical=%v, want docFail critical", c.status, c.critical)
		}
		if len(c.hints) == 0 || !strings.Contains(strings.Join(c.hints, " "), "m-embed") {
			t.Errorf("hints=%v, want one naming the model", c.hints)
		}
	})
}

// TestDoctorDeepEmbedHitsInference proves the embed deep probe sends a real
// /v1/embeddings request (not just a liveness metadata GET) and classifies a
// valid response as usable.
func TestDoctorDeepEmbedHitsInference(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))
	defer srv.Close()

	t.Setenv("DEX_EMBED_URL", srv.URL)
	t.Setenv("DEX_EMBED_MODEL", "test-embed")

	p := embedProbe(t)
	c := classifyDeep(p, p.deep(context.Background()))
	if c.status != docOK {
		t.Fatalf("deep embed vs valid server: status=%v detail=%q", c.status, c.detail)
	}
	if !strings.Contains(gotPath, "embeddings") {
		t.Errorf("deep embed hit %q, want an /embeddings inference path", gotPath)
	}
}

// TestDoctorDeepEmbedModelMissing: the server is reachable (liveness would pass)
// but rejects the embed call — deep must report a critical not-ready, distinct
// from unreachable.
func TestDoctorDeepEmbedModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"model \"absent\" not found"}}`)
	}))
	defer srv.Close()

	t.Setenv("DEX_EMBED_URL", srv.URL)
	t.Setenv("DEX_EMBED_MODEL", "absent")

	p := embedProbe(t)
	c := classifyDeep(p, p.deep(context.Background()))
	if c.status != docFail || !c.critical {
		t.Fatalf("model-missing embed: status=%v critical=%v, want docFail critical", c.status, c.critical)
	}
}

// embedProbe pulls the embed probe out of collectEndpoints and fails if it is
// not configured with a deep closure.
func embedProbe(t *testing.T) endpointProbe {
	t.Helper()
	for _, p := range collectEndpoints() {
		if p.name == "embed" {
			if p.deep == nil {
				t.Fatal("embed probe has no deep closure")
			}
			return p
		}
	}
	t.Fatal("embed probe not found")
	return endpointProbe{}
}
