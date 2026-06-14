package main

import (
	"testing"

	"github.com/alehatsman/dex/internal/mcp"
)

// TestReadModeParity locks the CLI `read --mode` set against the MCP `read`
// tool's mode set (issue #491). Every asymmetry must be deliberate and named
// here, so neither surface can silently drift from the other again.
//
// Two allow-lists capture the intentional asymmetries:
//
//   - mcpOnlyModes: MCP modes with no plain CLI --mode. `lines` is reachable on
//     the CLI through --start/--end (which become the `lines:N-M` request), so
//     it needs no --mode of its own.
//   - cliOnlyModes: CLI-local conveniences that never reach the MCP tool.
//     `entropy` and `auto` are local heuristics (auto mirrors the redirect
//     hook's signatures/full pivot); they spin up no index work an MCP caller
//     would want and so stay CLI-only.
//
// Note the MCP tool also exposes session-scoped extras with no mode of their
// own — `expand` (@B<n> body handles) and the internal `handle` downgrade
// terminal. Those are MCP-only by nature: body handles live in per-session
// server memory, so a fresh CLI process can't resolve a handle a prior one
// issued. They are excluded from ReadModes() and need no allow-list entry.
func TestReadModeParity(t *testing.T) {
	cli := make(map[string]bool, len(readModeChoices))
	for _, m := range readModeChoices {
		cli[m] = true
	}
	mcpModes := make(map[string]bool)
	for _, m := range mcp.ReadModes() {
		mcpModes[m] = true
	}

	// MCP modes reachable on the CLI directly, modulo the documented asymmetry.
	mcpOnlyModes := map[string]bool{"lines": true} // expressed via --start/--end
	for m := range mcpModes {
		if cli[m] || mcpOnlyModes[m] {
			continue
		}
		t.Errorf("MCP read mode %q has no CLI --mode and is not an allow-listed asymmetry", m)
	}

	// CLI modes that map onto an MCP mode, modulo the documented asymmetry.
	cliOnlyModes := map[string]bool{"entropy": true, "auto": true}
	for m := range cli {
		if mcpModes[m] || cliOnlyModes[m] {
			continue
		}
		t.Errorf("CLI read mode %q is neither an MCP mode nor an allow-listed CLI-only mode", m)
	}

	// Guard the allow-lists themselves: a stale entry (mode later unified or
	// removed) is drift too, so fail if an allow-listed mode is no longer real.
	for m := range mcpOnlyModes {
		if !mcpModes[m] {
			t.Errorf("mcpOnlyModes lists %q but it is not an MCP read mode — stale allow-list", m)
		}
	}
	for m := range cliOnlyModes {
		if !cli[m] {
			t.Errorf("cliOnlyModes lists %q but it is not a CLI read mode — stale allow-list", m)
		}
	}
}
