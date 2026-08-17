package mcp

import (
	"context"
	"encoding/json"
	"os"

	"github.com/alehatsman/dex/internal/proj"
)

// Shared warm cache (#171 swarm-context-spine S3): when one agent renders a
// compressed view of a file (signatures/skeleton/map/aggressive — tree-sitter,
// index lookups, text compression), a peer that reads the SAME file in the SAME
// mode reuses that render instead of paying the cold-start tax again. It rides
// the share_cache table (path+content_hash keyed, auto-evicts on hash change),
// so it is safe by construction: a changed file yields a new etag → a miss →
// a fresh render. A destructive `dex reindex` builds a new DB, dropping the
// cache for free. Best-effort throughout — any store error falls through to a
// normal render, never an error.

// worthWarmCaching reports whether a rendered read is expensive enough to be
// worth sharing across agents. Only the deterministic *compressed* renders
// qualify: raw full/lines are a cheap byte slice (sharing just duplicates bytes
// in sqlite), and summary is LLM output (non-deterministic — a warm hit would
// serve another model's take). That leaves signatures/skeleton/map/aggressive.
func worthWarmCaching(mode ReadMode) bool {
	return mode.IsLossySummary() && !mode.IsLLM()
}

// warmCacheHash is the render identity stored as share_cache.content_hash: the
// file-content etag combined with the read mode, since the cached value is the
// render of *this content* in *this mode*. Folding the mode here (rather than
// into the path) keeps share_cache.path a clean, inspectable file path and
// avoids embedding a separator in a UNIQUE TEXT column. Because share_cache is
// UNIQUE(path), only one mode's render is cached per file at a time — reading
// the same file in a second mode evicts the first (SharePull treats the hash
// mismatch as stale). That mode-thrash is acceptable: an agent working a file
// usually sticks to one compressed mode.
func warmCacheHash(etag string, mode ReadMode) string {
	return etag + "@" + mode.String()
}

// warmCacheEnabled is the kill switch: DEX_SWARM_WARMCACHE=off disables the
// shared warm cache entirely (default on). Cheap enough to check per read.
func warmCacheEnabled() bool {
	return os.Getenv("DEX_SWARM_WARMCACHE") != "off"
}

// warmCachePull returns a peer's cached render for (relTarget, mode) when the
// file content hash (etag) still matches, else ok=false. On a hit the rendered
// output is returned verbatim with a warm-cache marker appended to the hint.
func (s *Server) warmCachePull(ctx context.Context, p *proj.Project, relTarget string, mode ReadMode, etag string) (SummarizeOutput, bool) {
	if !warmCacheEnabled() || !worthWarmCaching(mode) || etag == "" {
		return SummarizeOutput{}, false
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return SummarizeOutput{}, false
	}
	content, _, ok, err := st.SharePull(ctx, relTarget, warmCacheHash(etag, mode))
	if err != nil || !ok {
		return SummarizeOutput{}, false
	}
	var out SummarizeOutput
	if json.Unmarshal([]byte(content), &out) != nil {
		return SummarizeOutput{}, false
	}
	out.Hint = appendWarmHint(out.Hint)
	return out, true
}

// warmCachePush stores a freshly-rendered compressed output for peers to reuse.
// Session-specific fields are stripped so the cached copy is agent-neutral.
// Best-effort: no store, wrong mode, or a non-ok render is silently skipped.
func (s *Server) warmCachePush(ctx context.Context, p *proj.Project, relTarget string, mode ReadMode, etag string, out SummarizeOutput) {
	if !warmCacheEnabled() || !worthWarmCaching(mode) || etag == "" {
		return
	}
	if out.Status != "ok" || out.Content == "" || out.SeenTurn != 0 {
		return // nothing useful, or a session-dedup suppression — never cache those
	}
	out.ScopedNotes = nil // path-bound, re-attached per read by the caller's defer
	out.SeenTurn = 0
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return
	}
	_ = st.SharePush(ctx, relTarget, warmCacheHash(etag, mode), string(b), s.AgentID)
}

// appendWarmHint marks a warm-cache hit so an agent (and the eval harness) can
// see the render came from a peer, not a fresh compression.
func appendWarmHint(hint string) string {
	const tag = "♻ warm-cache hit (reused a peer's compression)"
	if hint == "" {
		return tag
	}
	return hint + " · " + tag
}
