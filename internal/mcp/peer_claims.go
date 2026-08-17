package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// claimTTL bounds how long a peer's file claim stays live. Edits are active work
// — a claim older than this is treated as abandoned (the agent moved on or
// crashed without releasing) rather than a standing lock. Deliberately short:
// an over-eager caveat that outlives the edit is worse than a missed one.
const claimTTL = 30 * time.Minute

// claimProber lets look surface concurrent peer edits on a target file (#170
// S1). Implemented by *Server; the http projectScoped handler does not (claims
// are a local-swarm concern), so remote callers simply see no claim caveat.
type claimProber interface {
	peerClaimCaveat(ctx context.Context, root, target string) (string, bool)
}

// peerClaimCaveat returns a trust caveat when a *different* agent holds a fresh,
// unreleased claim on target, else ("", false). Best-effort: an unresolved
// project, missing index, or store error is silent — a claim caveat is advisory
// and must never turn a working look() into an error.
func (s *Server) peerClaimCaveat(ctx context.Context, root, target string) (string, bool) {
	p, hint := s.resolveProject(ctx, root)
	if hint != "" {
		return "", false
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		return "", false
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return "", false
	}
	claims, err := st.ClaimsOverlapping(ctx, target, s.AgentID, time.Now().Add(-claimTTL))
	if err != nil || len(claims) == 0 {
		return "", false
	}
	return formatClaimCaveat(claims), true
}

// formatClaimCaveat renders one or more active peer claims into a single
// caveat line, e.g. "peer alice is editing this file (adding AgentQueryVec, 3m ago)".
func formatClaimCaveat(claims []store.Claim) string {
	if len(claims) == 1 {
		c := claims[0]
		who := c.AgentID
		if c.Role != "" {
			who += "/" + c.Role
		}
		intent := c.Intent
		if intent == "" || intent == "editing" {
			intent = "editing"
		}
		return fmt.Sprintf("peer %s is editing this file (%s, %s ago) — coordinate before you overwrite",
			who, intent, humanizeClaimAge(time.Since(c.PostedAt)))
	}
	who := make([]string, 0, len(claims))
	for _, c := range claims {
		who = append(who, c.AgentID)
	}
	return fmt.Sprintf("%d peers are editing this file (%s) — coordinate before you overwrite",
		len(claims), strings.Join(who, ", "))
}

func humanizeClaimAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// flagPeerClaims adds a peer-edit caveat to a look result. It appends to any
// existing caveat (e.g. an index-rebuild note) rather than clobbering it, since
// both signals can be true at once. Trust.Fresh is left alone — a claim does not
// make the fetched bytes stale, it flags a coordination hazard.
func flagPeerClaims(ctx context.Context, h toolSurface, root, target string, out *LookOutput) {
	cp, ok := h.(claimProber)
	if !ok {
		return
	}
	caveat, ok := cp.peerClaimCaveat(ctx, root, target)
	if !ok {
		return
	}
	if out.Trust.Caveat == "" {
		out.Trust.Caveat = caveat
	} else {
		out.Trust.Caveat += " · " + caveat
	}
}
