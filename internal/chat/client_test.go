package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// closedURL returns an http:// URL pointing at a port that is guaranteed to
// refuse connections: bind a random port, record its address, close it, return
// the URL. This is more reliable than a hardcoded port like :1, which may have
// a process listening on some environments (e.g. WSL2).
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

func okHandler(reply string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		type choice struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}
		out := struct {
			Choices []choice `json:"choices"`
			Model   string   `json:"model"`
		}{
			Model: body.Model,
			Choices: []choice{{
				Message:      Message{Role: "assistant", Content: reply},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

func TestGenerateRoundTrip(t *testing.T) {
	srv := httptest.NewServer(okHandler("hello world"))
	defer srv.Close()
	c := New(srv.URL, "fake", 5*time.Second)
	resp, err := c.Generate(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello world" {
		t.Errorf("content = %q, want %q", resp.Content, "hello world")
	}
	if resp.Model != "fake" {
		t.Errorf("model = %q, want %q", resp.Model, "fake")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "stop")
	}
}

func TestGenerateUnreachable(t *testing.T) {
	c := New(closedURL(t), "fake", 200*time.Millisecond)
	_, err := c.Generate(context.Background(), []Message{{Role: "user", Content: "x"}}, Options{})
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("expected ErrUnreachable, got %v", err)
	}
}

func TestGenerateServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, "fake", 2*time.Second)
	_, err := c.Generate(context.Background(), []Message{{Role: "user", Content: "x"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected http 503 error, got %v", err)
	}
}

func TestGenerateNoMessages(t *testing.T) {
	c := New("http://example/", "m", time.Second)
	_, err := c.Generate(context.Background(), nil, Options{})
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func sseHandler(tokens []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, tok := range tokens {
			chunk := streamChunk{Model: "fake-stream"}
			chunk.Choices = []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			}{{Delta: struct {
				Content string `json:"content"`
			}{Content: tok}}}
			b, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
}

func TestGenerateStreamAssemblesTokens(t *testing.T) {
	srv := httptest.NewServer(sseHandler([]string{"hel", "lo ", "world"}))
	defer srv.Close()
	c := New(srv.URL, "fake", 5*time.Second)

	var got []string
	resp, err := c.GenerateStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}, func(tok string) {
		got = append(got, tok)
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello world" {
		t.Errorf("content = %q, want %q", resp.Content, "hello world")
	}
	if strings.Join(got, "") != "hello world" {
		t.Errorf("tokens = %v, joined = %q, want %q", got, strings.Join(got, ""), "hello world")
	}
	if resp.Model != "fake-stream" {
		t.Errorf("model = %q, want fake-stream", resp.Model)
	}
}

func TestGenerateStreamNilCallback(t *testing.T) {
	srv := httptest.NewServer(sseHandler([]string{"ok"}))
	defer srv.Close()
	c := New(srv.URL, "fake", 5*time.Second)
	resp, err := c.GenerateStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Content)
	}
}

func TestGenerateStreamNoMessages(t *testing.T) {
	c := New("http://example/", "m", time.Second)
	_, err := c.GenerateStream(context.Background(), nil, Options{}, nil)
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func TestNewDefaults(t *testing.T) {
	c := New("http://example/", "m", 0)
	if c.HTTP.Timeout != 120*time.Second {
		t.Errorf("Timeout default = %s, want 120s", c.HTTP.Timeout)
	}
	if strings.HasSuffix(c.BaseURL, "/") {
		t.Errorf("BaseURL should be trimmed: %q", c.BaseURL)
	}
}

// modelsHandler returns a /v1/models handler that lists the given model IDs.
func modelsHandler(ids ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		type modelEntry struct {
			ID string `json:"id"`
		}
		resp := struct {
			Data []modelEntry `json:"data"`
		}{}
		for _, id := range ids {
			resp.Data = append(resp.Data, modelEntry{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func TestHealthModelPresent(t *testing.T) {
	srv := httptest.NewServer(modelsHandler("llama3", "mistral"))
	defer srv.Close()
	c := New(srv.URL, "llama3", 2*time.Second)
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health with present model = %v, want nil", err)
	}
}

func TestHealthModelNotFound(t *testing.T) {
	srv := httptest.NewServer(modelsHandler("llama3", "mistral"))
	defer srv.Close()
	c := New(srv.URL, "gpt-4", 2*time.Second)
	err := c.Health(context.Background())
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("Health with absent model = %v, want ErrModelNotFound", err)
	}
}

func TestHealthUnreachable(t *testing.T) {
	c := New(closedURL(t), "fake", 200*time.Millisecond)
	err := c.Health(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("Health on closed port = %v, want ErrUnreachable", err)
	}
}

func TestHealthFailOpenUnparseable(t *testing.T) {
	// Server returns 200 but with non-JSON body — should fail open (return nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()
	c := New(srv.URL, "any-model", 2*time.Second)
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health with unparseable body = %v, want nil (fail-open)", err)
	}
}

func TestHealthFailOpenEmptyList(t *testing.T) {
	// Server returns {"data":[]} — empty list should fail open (return nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "any-model", 2*time.Second)
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health with empty data list = %v, want nil (fail-open)", err)
	}
}
