package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newOllamaStub(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		type model struct {
			Name string `json:"name"`
		}
		type response struct {
			Models []model `json:"models"`
		}
		body := response{}
		for _, name := range models {
			body.Models = append(body.Models, model{Name: name})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}))
}

func TestDetectOllamaFrom_NoModels(t *testing.T) {
	srv := newOllamaStub(t, nil)
	defer srv.Close()
	_, ok := detectOllamaFrom(context.Background(), srv.URL)
	if ok {
		t.Fatal("expected false with no models")
	}
}

func TestDetectOllamaFrom_SingleEmbedModel(t *testing.T) {
	srv := newOllamaStub(t, []string{"nomic-embed-text:latest"})
	defer srv.Close()
	m, ok := detectOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected model to be found")
	}
	if m.Name != "nomic-embed-text:latest" {
		t.Fatalf("got %q, want nomic-embed-text:latest", m.Name)
	}
	if m.URL != srv.URL {
		t.Fatalf("got URL %q, want %q", m.URL, srv.URL)
	}
}

func TestDetectOllamaFrom_NonEmbedModelsIgnored(t *testing.T) {
	srv := newOllamaStub(t, []string{"llama3.2:3b", "mistral:7b"})
	defer srv.Close()
	_, ok := detectOllamaFrom(context.Background(), srv.URL)
	if ok {
		t.Fatal("expected false: only non-embed models present")
	}
}

func TestDetectOllamaFrom_PicksHighestPriority(t *testing.T) {
	// mxbai-embed-large ranks above nomic-embed-text and all-minilm.
	srv := newOllamaStub(t, []string{
		"all-minilm:latest",
		"llama3.2:3b",
		"nomic-embed-text:latest",
		"mxbai-embed-large:latest",
	})
	defer srv.Close()
	m, ok := detectOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected model to be found")
	}
	if m.Name != "mxbai-embed-large:latest" {
		t.Fatalf("got %q, want mxbai-embed-large:latest", m.Name)
	}
}

func TestDetectOllamaFrom_Unreachable(t *testing.T) {
	// Port 1 is reserved; connect will be refused immediately.
	_, ok := detectOllamaFrom(context.Background(), "http://127.0.0.1:1")
	if ok {
		t.Fatal("expected false for unreachable server")
	}
}

func TestDetectOllamaFrom_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, ok := detectOllamaFrom(context.Background(), srv.URL)
	if ok {
		t.Fatal("expected false for non-200 response")
	}
}

func TestEmbedModelPriority_KnownModels(t *testing.T) {
	cases := []struct {
		name    string
		wantPos bool
	}{
		{"mxbai-embed-large:latest", true},
		{"nomic-embed-text:latest", true},
		{"bge-m3:latest", true},
		{"all-minilm:latest", true},
		{"llama3.2:3b", false},
		{"mistral:7b-instruct", false},
		{"qwen2.5-coder:7b", false},
	}
	for _, tc := range cases {
		pri := embedModelPriority(tc.name)
		if tc.wantPos && pri < 0 {
			t.Errorf("%s: want positive priority, got %d", tc.name, pri)
		}
		if !tc.wantPos && pri >= 0 {
			t.Errorf("%s: want negative priority, got %d", tc.name, pri)
		}
	}
}

func TestEmbedModelPriority_Ordering(t *testing.T) {
	// Higher-quality models must rank above lower-quality ones.
	pairs := [][2]string{
		{"mxbai-embed-large", "nomic-embed-text"},
		{"nomic-embed-text", "all-minilm"},
		{"bge-large", "bge-base"},
	}
	for _, p := range pairs {
		hi, lo := embedModelPriority(p[0]), embedModelPriority(p[1])
		if hi <= lo {
			t.Errorf("%s (pri %d) should outrank %s (pri %d)", p[0], hi, p[1], lo)
		}
	}
}
