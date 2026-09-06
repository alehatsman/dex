package mcp

import (
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
)

// TestClonesSimilarGating guards the exposure contract for `search` (#84,
// #142): embedder-backed AND power-lane, so it appears only when an embedder
// is wired *and* DEX_EXPERT is set. (search was demoted from the everyday
// surface in #142 — everyday concept-search is covered by ask(behavior_search).)
//
// clones/similar are no longer standalone tools at all (#852,
// query-unification MCP re-justification) — folded into
// `query(kind=clones|similar)`, see TestClonesSimilarNotStandaloneTools below.
func TestClonesSimilarGating(t *testing.T) {
	withEmbed := func(t *testing.T) *Server {
		s := stubServer(t)
		s.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)
		return s
	}

	// embedder present, expert off → gated away.
	t.Setenv("DEX_EXPERT", "")
	names := listToolNames(t, withEmbed(t))
	if names["search"] {
		t.Error(`"search" advertised without DEX_EXPERT; want power-lane gated`)
	}

	// embedder present, expert on → advertised.
	t.Setenv("DEX_EXPERT", "1")
	names = listToolNames(t, withEmbed(t))
	if !names["search"] {
		t.Error(`"search" not advertised with DEX_EXPERT=1 and an embedder`)
	}

	// no embedder, expert on → still gated away (vectors required).
	names = listToolNames(t, stubServer(t))
	if names["search"] {
		t.Error(`"search" advertised without an embedder; want embed-gated`)
	}
}

// TestClonesSimilarNotStandaloneTools locks the #852 invariant: clones/similar
// are reachable only via query(kind=clones|similar), never as their own
// top-level MCP tools, in any mode (default, DEX_EXPERT, with or without an
// embedder) — guards against the surface silently regrowing a duplicate door.
func TestClonesSimilarNotStandaloneTools(t *testing.T) {
	s := stubServer(t)
	s.EmbedClient = embed.New("http://127.0.0.1:0", "fake", 16, 200*time.Millisecond)
	for _, expert := range []string{"", "1"} {
		t.Setenv("DEX_EXPERT", expert)
		names := listToolNames(t, s)
		for _, n := range []string{"clones", "similar"} {
			if names[n] {
				t.Errorf("DEX_EXPERT=%q: %q advertised as a standalone tool; want it reachable only via query(kind=%s)", expert, n, n)
			}
		}
	}
}
