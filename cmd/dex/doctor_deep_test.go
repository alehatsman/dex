package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/health"
)

// TestDoctorDeepEmbedHitsInference proves the embed deep probe (as wired by
// collectEndpoints) sends a real /v1/embeddings request — not just a liveness
// metadata GET — and that health.ClassifyDeep calls a valid response usable.
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
	c := health.ClassifyDeep(p, p.Deep(context.Background()))
	if c.Status != health.OK {
		t.Fatalf("deep embed vs valid server: status=%v detail=%q", c.Status, c.Detail)
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
	c := health.ClassifyDeep(p, p.Deep(context.Background()))
	if c.Status != health.Fail || !c.Critical {
		t.Fatalf("model-missing embed: status=%v critical=%v, want Fail critical", c.Status, c.Critical)
	}
}

// embedProbe pulls the embed probe out of collectEndpoints and fails if it is
// not configured with a deep closure.
func embedProbe(t *testing.T) health.Probe {
	t.Helper()
	for _, p := range collectEndpoints() {
		if p.Name == "embed" {
			if p.Deep == nil {
				t.Fatal("embed probe has no deep closure")
			}
			return p
		}
	}
	t.Fatal("embed probe not found")
	return health.Probe{}
}
