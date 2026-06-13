package main

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/rerank"
)

func TestNewRerankClientNilWhenURLEmpty(t *testing.T) {
	t.Setenv("DEX_RERANK_URL", "")
	if c := newRerankClient(); c != nil {
		t.Errorf("newRerankClient() = %+v, want nil when URL unset", c)
	}
}

func TestNewRerankClientReturnsClientWhenURLSet(t *testing.T) {
	t.Setenv("DEX_RERANK_URL", "http://127.0.0.1:9999")
	t.Setenv("DEX_RERANK_MODEL", "custom-reranker")
	t.Setenv("DEX_DISABLE_RERANK", "")
	t.Setenv("DEX_RERANK_STYLE", "cohere")

	c := newRerankClient()
	if c == nil {
		t.Fatal("newRerankClient() = nil, want non-nil when URL is set")
	}
	if c.Endpoint() != "http://127.0.0.1:9999" {
		t.Errorf("Endpoint() = %q, want http://127.0.0.1:9999", c.Endpoint())
	}
	if c.ModelName() != "custom-reranker" {
		t.Errorf("ModelName() = %q, want custom-reranker", c.ModelName())
	}
}

func TestNewRerankClientChatStyleWhenRequested(t *testing.T) {
	t.Setenv("DEX_RERANK_URL", "http://127.0.0.1:9999")
	t.Setenv("DEX_RERANK_MODEL", "Qwen/Qwen3-Reranker-4B")
	t.Setenv("DEX_RERANK_STYLE", "chat")
	t.Setenv("DEX_DISABLE_RERANK", "")

	c := newRerankClient()
	if c == nil {
		t.Fatal("newRerankClient() = nil, want non-nil when URL is set")
	}
	if _, ok := c.(*rerank.ChatReranker); !ok {
		t.Errorf("expected *rerank.ChatReranker, got %T", c)
	}
	if c.Endpoint() != "http://127.0.0.1:9999" {
		t.Errorf("Endpoint() = %q, want http://127.0.0.1:9999", c.Endpoint())
	}
}

func TestNewRerankClientDefaultsToQwen3ChatVLLM(t *testing.T) {
	t.Setenv("DEX_RERANK_URL", "http://127.0.0.1:9999")
	t.Setenv("DEX_RERANK_STYLE", "")
	t.Setenv("DEX_RERANK_MODEL", "")
	t.Setenv("DEX_DISABLE_RERANK", "")

	c := newRerankClient()
	if c == nil {
		t.Fatal("newRerankClient() = nil, want non-nil when URL is set")
	}
	if _, ok := c.(*rerank.ChatReranker); !ok {
		t.Errorf("expected *rerank.ChatReranker (chat-vllm default), got %T", c)
	}
	if c.ModelName() != "Qwen/Qwen3-Reranker-4B" {
		t.Errorf("ModelName() = %q, want Qwen/Qwen3-Reranker-4B", c.ModelName())
	}
}

func TestNewRerankClientNilWhenDisableSet(t *testing.T) {
	// URL is set, but the kill switch is on. nil should still be returned.
	t.Setenv("DEX_RERANK_URL", "http://127.0.0.1:9999")
	t.Setenv("DEX_DISABLE_RERANK", "1")

	if c := newRerankClient(); c != nil {
		t.Errorf("newRerankClient() = %+v, want nil when DISABLE_RERANK=1", c)
	}
}

func TestNewRerankClientDefaultTimeout(t *testing.T) {
	t.Setenv("DEX_RERANK_URL", "http://127.0.0.1:9999")
	t.Setenv("DEX_RERANK_TIMEOUT", "")
	t.Setenv("DEX_DISABLE_RERANK", "")
	t.Setenv("DEX_RERANK_STYLE", "cohere") // access concrete *rerank.Client type

	c := newRerankClient()
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	cc, ok := c.(*rerank.Client)
	if !ok {
		t.Fatalf("expected *rerank.Client, got %T", c)
	}
	if cc.HTTP.Timeout != 5*time.Second {
		t.Errorf("default timeout = %s, want 5s", cc.HTTP.Timeout)
	}
}

func TestRerankPoolDefault(t *testing.T) {
	t.Setenv("DEX_RERANK_POOL", "")
	if got := rerankPool(); got != 40 {
		t.Errorf("rerankPool() = %d, want 40 (default)", got)
	}
}

func TestRerankPoolHonoredInRange(t *testing.T) {
	t.Setenv("DEX_RERANK_POOL", "60")
	if got := rerankPool(); got != 60 {
		t.Errorf("rerankPool() = %d, want 60", got)
	}
}

func TestRerankPoolClampsHigh(t *testing.T) {
	t.Setenv("DEX_RERANK_POOL", "9999")
	if got := rerankPool(); got != 100 {
		t.Errorf("rerankPool() = %d, want 100 (clamped)", got)
	}
}

func TestRerankPoolFallbackOnInvalid(t *testing.T) {
	t.Setenv("DEX_RERANK_POOL", "not-an-int")
	if got := rerankPool(); got != 40 {
		t.Errorf("rerankPool() = %d, want 40 (fallback after warning)", got)
	}
}

func TestRerankPoolFallbackOnNonPositive(t *testing.T) {
	t.Setenv("DEX_RERANK_POOL", "0")
	if got := rerankPool(); got != 40 {
		t.Errorf("rerankPool() = %d, want 40 (zero falls back)", got)
	}
	t.Setenv("DEX_RERANK_POOL", "-5")
	if got := rerankPool(); got != 40 {
		t.Errorf("rerankPool() = %d, want 40 (negative falls back)", got)
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		raw  string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"1", false, true}, {"on", false, true}, {"true", false, true}, {"yes", false, true},
		{"0", true, false}, {"off", true, false}, {"false", true, false}, {"no", true, false},
		{"weird", false, false}, // unknown values fall back to def
		{"weird", true, true},
		{"  ON  ", false, true}, // trimmed + case-insensitive
	}
	for _, c := range cases {
		t.Setenv("DEX_TEST_BOOL", c.raw)
		got := envBool("DEX_TEST_BOOL", c.def)
		if got != c.want {
			t.Errorf("envBool(%q, def=%v) = %v, want %v", c.raw, c.def, got, c.want)
		}
	}
}
