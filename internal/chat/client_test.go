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
		http.Error(w, "model overloaded", 503)
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
