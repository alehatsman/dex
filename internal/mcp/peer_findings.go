package mcp

import (
	"context"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// peerFindingTTL bounds how long a peer's bus finding stays foldable. Findings
// are ephemeral swarm signals, not durable knowledge (#180 pragmatic-hybrid
// lifecycle): a finding that keeps proving useful graduates to a real fact via
// the normal remember/supersede path; one that goes quiet ages out here rather
// than accreting. Recency is measured from posted_at.
const peerFindingTTL = 24 * time.Hour

// maxPeerFindings caps how many peer findings ask folds into one pack. Kept
// small: these are lower-trust, clearly-tagged additions that must not crowd out
// durable knowledge or the evidence pack itself.
const maxPeerFindings = 3

// foldPeerFindings folds category=finding messages a *peer* agent posted to the
// shared bus into the ask() pack's knowledge_facts slot — the read half of the
// swarm findings bus (#180 / swarm-context-spine S2, promoting the #168 spike).
//
// Recall is by vector similarity (AgentQueryVec) so a natural-language question
// surfaces a terse peer finding it shares no keywords with — the gap the spike
// hit with FTS implicit-AND. When no embedder is configured
// (DEX_EMBED_ENGINE=none) it falls back to FTS OR-matching (AgentReadAny).
//
// Each folded finding is:
//   - self-filtered: this process never re-surfaces its own findings (AgentID);
//   - TTL-bounded: older than peerFindingTTL is dropped (ephemeral lifecycle);
//   - tagged [peer-agent:<id>] so the agent can tell an unverified peer signal
//     from a durable fact ([<archetype>]); and
//   - liveness-checked (#167 Part 2): a finding naming a now-dead code referent
//     carries a ⚠ needs-verification note rather than reading as ground truth.
func (s *Server) foldPeerFindings(ctx context.Context, st *store.Store, in ContextInput, out *ContextOutput) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return
	}

	// Recall a wider pool than we keep — self-filter and TTL will thin it.
	pool := maxPeerFindings * 4
	var findings []store.AgentMessage
	if s.EmbedClient != nil {
		if vecs, err := s.EmbedClient.Embed(ctx, []string{q}); err == nil && len(vecs) > 0 {
			findings, _ = st.AgentQueryVec(ctx, vecs[0], "finding", pool, 0.25)
		}
	}
	if len(findings) == 0 { // no embedder, or vector recall came up empty → FTS-OR fallback
		findings, _ = st.AgentReadAny(ctx, "", "finding", q, 0, pool)
	}
	if len(findings) == 0 {
		return
	}

	// Liveness scaffolding, built once (mirrors annotateLiveness). Empty index →
	// paths is empty and we simply skip the dead-referent check.
	paths, _ := st.CodeFilePaths(ctx)
	var exts map[string]bool
	var symbolLive func(string) bool
	if len(paths) > 0 {
		exts = indexedExts(paths)
		symbolLive = func(name string) bool {
			hits, err := st.FindSymbol(ctx, name, 1)
			return err != nil || len(hits) > 0
		}
	}

	cutoff := time.Now().Add(-peerFindingTTL)
	kept := 0
	for _, m := range findings {
		if kept >= maxPeerFindings {
			break
		}
		if s.AgentID != "" && m.AgentID == s.AgentID {
			continue // never fold our own findings back to ourselves
		}
		if !m.PostedAt.IsZero() && m.PostedAt.Before(cutoff) {
			continue // aged out
		}
		body := capFactBody(m.Body)
		if len(paths) > 0 {
			if note := deadReferentNote(extractReferents(m.Body), paths, exts, symbolLive); note != "" {
				body += " ⚠ needs verification: " + note
			}
		}
		out.KnowledgeFacts = append(out.KnowledgeFacts, "[peer-agent:"+peerLabel(m)+"] "+body)
		kept++
	}
}

// peerLabel is the provenance handle shown in the fold tag: the agent id, plus
// its role when one was announced (DEX_AGENT_ROLE), e.g. "agent-1a2b/reviewer".
func peerLabel(m store.AgentMessage) string {
	if m.Role != "" {
		return m.AgentID + "/" + m.Role
	}
	return m.AgentID
}
