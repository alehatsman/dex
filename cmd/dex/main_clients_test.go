package main

import (
	"os"
	"testing"
)

// TestNewChatClientConfigured pins the reporting contract behind #133: chat is
// "configured" only when a chat model was actually wired. A bare DEX_CHAT_URL
// (often a shared ollama endpoint that only serves embeddings) is NOT enough —
// without a model we fall back to a fabricated default and must report "not
// configured" rather than DEGRADED against a model the user never asked for.
//
// Both cases set DEX_CHAT_URL so the ollama auto-detect branch (network +
// best-effort daemon start) is never taken — the test stays hermetic.
func TestNewChatClientConfigured(t *testing.T) {
	// Isolate from the ambient environment.
	for _, k := range []string{"DEX_CHAT_URL", "DEX_CHAT_MODEL"} {
		old, had := os.LookupEnv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}

	t.Run("url set, model unset -> not configured", func(t *testing.T) {
		os.Setenv("DEX_CHAT_URL", "http://127.0.0.1:11434")
		os.Unsetenv("DEX_CHAT_MODEL")

		c, configured := newChatClientConfigured()
		if configured {
			t.Error("configured = true; a bare DEX_CHAT_URL without a model must report not-configured")
		}
		if c == nil {
			t.Fatal("client is nil; ask/summary must still degrade gracefully via the default")
		}
		// The fabricated default is still present so the client is usable.
		if c.Model == "" {
			t.Error("default model was not applied")
		}
	})

	t.Run("explicit model -> configured", func(t *testing.T) {
		os.Setenv("DEX_CHAT_URL", "http://127.0.0.1:11434")
		os.Setenv("DEX_CHAT_MODEL", "qwen2.5-coder:14b")

		c, configured := newChatClientConfigured()
		if !configured {
			t.Error("configured = false; an explicit DEX_CHAT_MODEL must report configured")
		}
		if c == nil {
			t.Fatal("client is nil")
		}
		if c.Model != "qwen2.5-coder:14b" {
			t.Errorf("Model = %q, want the explicit model", c.Model)
		}
	})
}
