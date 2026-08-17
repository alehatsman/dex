// Swarm identity (#180): how one dex process names itself on the shared
// findings bus. Both the long-lived MCP server and a one-shot `dex agent post`
// (typically spawned via `act`, inheriting the session env) must agree on the
// same identity, or an agent's own findings won't be self-filtered out of its
// recall fold. The env override is the cross-process anchor; the random mint is
// the zero-config default for a lone process.
//
// Why not the MCP session id? Claude Code's stdio transport returns an empty
// Session.ID() (dex falls back to a fixed "stdio" sentinel — see
// internal/mcp/seen.go), so it can't distinguish concurrent agents. The real
// identity boundary is the OS process meeting at the shared SQLite DB.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

// agentIdentity resolves this process's swarm identity: DEX_AGENT_ID if set,
// otherwise a random per-process id (stable for the life of the process).
// role is DEX_AGENT_ROLE (may be empty). Maps 1:1 onto AgentAnnounce(id, role).
func agentIdentity() (id, role string) {
	role = os.Getenv("DEX_AGENT_ROLE")
	if v := os.Getenv("DEX_AGENT_ID"); v != "" {
		return v, role
	}
	return "agent-" + randHex(4), role
}

// randHex returns n random bytes hex-encoded (2n chars). Falls back to a fixed
// token only if the OS RNG fails, which effectively never happens.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "anon"
	}
	return hex.EncodeToString(b)
}
