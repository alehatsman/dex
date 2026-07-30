package mcp

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
)

// TestClonesSimilarGating guards the exposure contract for the duplication
// lanes (#84): `clones` and `similar` are vector-backed AND power-lane, so they
// appear only when an embedder is wired *and* DEX_EXPERT is set.
func TestClonesSimilarGating(t *testing.T) {
	withEmbed := func(t *testing.T) *Server {
		s := stubServer(t)
		s.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)
		return s
	}

	// embedder present, expert off → gated away.
	t.Setenv("DEX_EXPERT", "")
	names := listToolNames(t, withEmbed(t))
	for _, n := range []string{"clones", "similar"} {
		if names[n] {
			t.Errorf("%q advertised without DEX_EXPERT; want power-lane gated", n)
		}
	}

	// embedder present, expert on → advertised.
	t.Setenv("DEX_EXPERT", "1")
	names = listToolNames(t, withEmbed(t))
	for _, n := range []string{"clones", "similar"} {
		if !names[n] {
			t.Errorf("%q not advertised with DEX_EXPERT=1 and an embedder", n)
		}
	}

	// no embedder, expert on → still gated away (vectors required).
	names = listToolNames(t, stubServer(t))
	for _, n := range []string{"clones", "similar"} {
		if names[n] {
			t.Errorf("%q advertised without an embedder; want embed-gated", n)
		}
	}
}
