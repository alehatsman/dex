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

// ScanOllama tests

func TestScanOllamaFrom_ReachableNoEmbedModels(t *testing.T) {
	// Ollama is running but only has chat/LLM models — scan returns true with empty EmbedModels.
	srv := newOllamaStub(t, []string{"llama3.2:3b", "mistral:7b"})
	defer srv.Close()
	scan, ok := scanOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected true: ollama is reachable")
	}
	if len(scan.EmbedModels) != 0 {
		t.Fatalf("expected empty EmbedModels, got %v", scan.EmbedModels)
	}
	if scan.URL != srv.URL {
		t.Fatalf("URL mismatch: got %q", scan.URL)
	}
}

func TestScanOllamaFrom_ReturnsAllEmbedModelsPriorityOrdered(t *testing.T) {
	srv := newOllamaStub(t, []string{
		"all-minilm:latest",
		"llama3.2:3b",
		"mxbai-embed-large:latest",
		"nomic-embed-text:latest",
	})
	defer srv.Close()
	scan, ok := scanOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected true")
	}
	if len(scan.EmbedModels) != 3 {
		t.Fatalf("expected 3 embed models, got %d: %v", len(scan.EmbedModels), scan.EmbedModels)
	}
	// First entry must be the highest-priority model.
	if scan.EmbedModels[0] != "mxbai-embed-large:latest" {
		t.Fatalf("first model: got %q, want mxbai-embed-large:latest", scan.EmbedModels[0])
	}
	// Last entry must be the lowest-priority embed model.
	if scan.EmbedModels[2] != "all-minilm:latest" {
		t.Fatalf("last model: got %q, want all-minilm:latest", scan.EmbedModels[2])
	}
}

func TestScanOllamaFrom_Unreachable(t *testing.T) {
	_, ok := scanOllamaFrom(context.Background(), "http://127.0.0.1:1")
	if ok {
		t.Fatal("expected false for unreachable server")
	}
}
