package main

import "testing"

// TestNewEmbedClientNone proves the lean serve signal: DEX_EMBED_ENGINE=none
// yields a nil embedder, which the MCP server reads as "no embedder wired" and
// uses to omit the embedding-backed tools (#290). Any other value must keep
// returning a live client.
func TestNewEmbedClientNone(t *testing.T) {
	t.Setenv("DEX_EMBED_ENGINE", "none")
	if em := newEmbedClient(""); em != nil {
		t.Fatalf("DEX_EMBED_ENGINE=none: want nil embedder (lean profile), got %T", em)
	}

	// Case-insensitive, to match the strings.ToLower switch.
	t.Setenv("DEX_EMBED_ENGINE", "NONE")
	if em := newEmbedClient(""); em != nil {
		t.Fatalf("DEX_EMBED_ENGINE=NONE: want nil embedder, got %T", em)
	}

	// Default (http) still returns a live client.
	t.Setenv("DEX_EMBED_ENGINE", "")
	if em := newEmbedClient(""); em == nil {
		t.Fatal("DEX_EMBED_ENGINE unset: want a live HTTP embedder, got nil")
	}
}
