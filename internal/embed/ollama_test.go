package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newOllamaStub serves /api/tags with models and /v1/embeddings with a dummy
// vector. All listed models are considered live (health probes succeed).
func newOllamaStub(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	return newOllamaStubWithLive(t, models, nil)
}

// newOllamaStubWithLive serves /api/tags with models but only accepts
// /v1/embeddings for models in liveModels (nil = all live).
func newOllamaStubWithLive(t *testing.T, models []string, liveModels map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
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
		case "/v1/embeddings":
			if liveModels != nil {
				var req struct {
					Model string `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
				if !liveModels[req.Model] {
					http.Error(w, "model not found", http.StatusNotFound)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			// Minimal valid OpenAI-shape embeddings response.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"data": []map[string]any{{"embedding": []float32{0.1}, "index": 0}},
			})
		default:
			http.NotFound(w, r)
		}
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
	// qwen3-embedding ranks above mxbai-embed-large, nomic-embed-text, and all-minilm.
	srv := newOllamaStub(t, []string{
		"all-minilm:latest",
		"llama3.2:3b",
		"nomic-embed-text:latest",
		"mxbai-embed-large:latest",
		"qwen3-embedding:4b",
	})
	defer srv.Close()
	m, ok := detectOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected model to be found")
	}
	if m.Name != "qwen3-embedding:4b" {
		t.Fatalf("got %q, want qwen3-embedding:4b", m.Name)
	}
}

func TestDetectOllamaFrom_SkipsUnhealthyModel(t *testing.T) {
	// nomic-embed-text is listed but cannot serve embeds; qwen3-embedding can.
	// detectOllamaFrom must skip the broken model and return the working one.
	srv := newOllamaStubWithLive(t,
		[]string{"nomic-embed-text:latest", "qwen3-embedding:4b"},
		map[string]bool{"qwen3-embedding:4b": true},
	)
	defer srv.Close()
	m, ok := detectOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected a live model to be found")
	}
	if m.Name != "qwen3-embedding:4b" {
		t.Fatalf("got %q, want qwen3-embedding:4b", m.Name)
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
		{"qwen3-embedding:4b", true},
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
		{"qwen3-embedding", "mxbai-embed-large"},
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

func TestScanOllamaFrom_ChatAndEmbedSeparated(t *testing.T) {
	// embed models must not leak into ChatModels; chat models must not leak into EmbedModels.
	srv := newOllamaStub(t, []string{
		"nomic-embed-text:latest",
		"qwen2.5-coder:7b",
		"llama3.2:3b",
		"mxbai-embed-large:latest",
	})
	defer srv.Close()
	scan, ok := scanOllamaFrom(context.Background(), srv.URL)
	if !ok {
		t.Fatal("expected true")
	}
	if len(scan.EmbedModels) != 2 {
		t.Fatalf("embed: want 2, got %d: %v", len(scan.EmbedModels), scan.EmbedModels)
	}
	if len(scan.ChatModels) != 2 {
		t.Fatalf("chat: want 2, got %d: %v", len(scan.ChatModels), scan.ChatModels)
	}
	// qwen2.5-coder should rank above llama3.2
	if scan.ChatModels[0] != "qwen2.5-coder:7b" {
		t.Fatalf("chat[0]: want qwen2.5-coder:7b, got %q", scan.ChatModels[0])
	}
	// mxbai embed model must not appear in ChatModels
	for _, m := range scan.ChatModels {
		if m == "nomic-embed-text:latest" || m == "mxbai-embed-large:latest" {
			t.Errorf("embed model %q leaked into ChatModels", m)
		}
	}
}

func TestChatModelPriority(t *testing.T) {
	if chatModelPriority("qwen2.5-coder:7b") <= chatModelPriority("llama3.2:3b") {
		t.Error("qwen2.5-coder should outrank llama3.2")
	}
	if chatModelPriority("nomic-embed-text:latest") >= 0 {
		t.Error("embed model should not be recognised as chat model")
	}
	if chatModelPriority("unknown-model:latest") >= 0 {
		t.Error("unknown model should return -1")
	}
}
