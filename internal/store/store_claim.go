package store

import (
	"context"
	"strings"
	"time"
)

// Claims (#170 swarm-context-spine S1) ride the coordination bus as
// category=claim messages: topic = the claimed file, body = the edit intent.
// They are advisory — a peer's look() surfaces them as a trust caveat, never a
// lock. There is no new table; a claim is just a bus message with a reserved
// category, so it inherits the same TTL/prune lifecycle as findings.

// ClaimReleaseMarker is the body of a tombstone claim posted by
// `dex agent release`. The active-claims query treats it as a retraction: the
// latest claim per (agent, file) wins, so a release supersedes an earlier claim.
const ClaimReleaseMarker = "\x00released"

// NormalizeClaimPath canonicalizes a claim topic / look target for overlap
// matching: trims surrounding space, a leading "./", and a trailing ":<line>"
// suffix. It deliberately does NOT resolve against a repo root (the store has no
// root) — overlap uses suffix comparison so relative and absolute forms still
// match (claimsOverlap).
func NormalizeClaimPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	// strip a trailing :123 line suffix if present
	if i := strings.LastIndexByte(p, ':'); i > 0 {
		if line := p[i+1:]; line != "" && isAllDigits(line) {
			p = p[:i]
		}
	}
	return p
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Claim is one active (non-released) file claim.
type Claim struct {
	AgentID  string
	Role     string
	File     string // the claimed path (normalized as posted)
	Intent   string
	PostedAt time.Time
}

// ActiveClaims returns the currently-active file claims posted at or after
// `since` (the TTL cutoff), one per (agent, file) — the latest wins, and a
// latest that is a release tombstone drops the claim entirely. Ordered newest
// first. `since` is the caller's freshness policy; pass the zero time for all.
func (s *Store) ActiveClaims(ctx context.Context, since time.Time) ([]Claim, error) {
	q := `SELECT m.agent_id, COALESCE(a.role,''), m.topic, m.body, m.posted_at
	        FROM agent_messages m LEFT JOIN agents a ON a.id=m.agent_id
	       WHERE m.category='claim'`
	args := []any{}
	if !since.IsZero() {
		q += " AND m.posted_at >= ?"
		args = append(args, since.UnixNano())
	}
	q += " ORDER BY m.id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]bool) // agent\x00file → already resolved (latest wins)
	var out []Claim
	for rows.Next() {
		var agentID, role, file, body string
		var postedAt int64
		if err := rows.Scan(&agentID, &role, &file, &body, &postedAt); err != nil {
			return nil, err
		}
		key := agentID + "\x00" + file
		if seen[key] {
			continue // an older claim on the same file by the same agent
		}
		seen[key] = true
		if body == ClaimReleaseMarker {
			continue // latest is a release → not active
		}
		out = append(out, Claim{
			AgentID:  agentID,
			Role:     role,
			File:     file,
			Intent:   body,
			PostedAt: time.Unix(0, postedAt),
		})
	}
	return out, rows.Err()
}

// ClaimsOverlapping returns active claims (fresh since `since`) whose file
// overlaps target, excluding those held by selfID. Overlap is suffix-based so
// "internal/x.go", "./internal/x.go", and "/abs/repo/internal/x.go" all match.
func (s *Store) ClaimsOverlapping(ctx context.Context, target, selfID string, since time.Time) ([]Claim, error) {
	all, err := s.ActiveClaims(ctx, since)
	if err != nil {
		return nil, err
	}
	tgt := NormalizeClaimPath(target)
	if tgt == "" {
		return nil, nil
	}
	var hits []Claim
	for _, c := range all {
		if c.AgentID == selfID {
			continue
		}
		if claimsOverlap(c.File, tgt) {
			hits = append(hits, c)
		}
	}
	return hits, nil
}

// claimsOverlap reports whether a claimed file and a look target refer to the
// same file, tolerating relative/absolute and repo-rooted differences via a
// path-component suffix match.
func claimsOverlap(claim, target string) bool {
	claim = NormalizeClaimPath(claim)
	if claim == "" || target == "" {
		return false
	}
	if claim == target {
		return true
	}
	return pathSuffix(claim, target) || pathSuffix(target, claim)
}

// pathSuffix reports whether `long` ends with `short` on a path-separator
// boundary (so "a/b/c.go" has suffix "b/c.go" but not "c.go"-of-"xc.go").
func pathSuffix(long, short string) bool {
	if !strings.HasSuffix(long, short) {
		return false
	}
	if len(long) == len(short) {
		return true
	}
	return long[len(long)-len(short)-1] == '/'
}
