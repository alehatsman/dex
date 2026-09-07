package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/health"
)

// Codex CLI support for `dex doctor` (#844). Codex keeps its MCP registry in
// $CODEX_HOME/config.toml (default ~/.codex/config.toml) and, unlike Claude
// Code, does not surface an MCP server's `instructions` to the model — so
// `dex setup` also materializes the tool mapping into ~/.codex/AGENTS.md.
// These checks are read-only: the TOML is scanned, never parsed or rewritten,
// so dex takes no TOML dependency and `codex mcp add` stays the only writer.

// checkCodexMCPWiring reports whether dex is registered in Codex's config.toml.
// Gated by the caller on codex being installed, so an absent codex never adds a
// line; here it returns docWarn (non-critical — Codex is opt-in) when codex is
// present but unwired, and folds an env-snapshot drift warning into the hints.
func checkCodexMCPWiring() health.Check {
	where := codexMCPLocation()
	if where == "" {
		args := append([]string{"mcp", "add", "dex"}, codexAddArgsTail()...)
		return health.Check{
			Name:   "mcp(codex)",
			Status: health.Warn,
			Detail: "dex not found in Codex MCP configuration",
			Hints:  []string{"run: codex " + strings.Join(args, " ")},
		}
	}
	c := health.Check{Name: "mcp(codex)", Status: health.OK, Detail: "configured (" + where + ")"}
	if stale := codexEnvDrift(); len(stale) > 0 {
		c.Status = health.Warn
		c.Detail = "configured (" + where + "); env snapshot may be stale: " + strings.Join(stale, ", ")
		c.Hints = []string{"re-run `dex setup --agent=codex` after `codex mcp remove dex` to refresh --env"}
	}
	return c
}

// codexAddArgsTail builds the trailing portion of a `codex mcp add` command line
// (the --env snapshot plus `-- dex mcp`) for display in hints.
func codexAddArgsTail() []string {
	var args []string
	for _, kv := range codexEnvSnapshot() {
		args = append(args, "--env", kv)
	}
	return append(args, "--", "dex", "mcp")
}

// codexConfigPath returns $CODEX_HOME/config.toml, else ~/.codex/config.toml.
func codexConfigPath() string {
	base := os.Getenv("CODEX_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".codex")
	}
	return filepath.Join(base, "config.toml")
}

// codexMCPLocation returns a short description of where dex MCP is configured in
// Codex's config.toml, or "" if absent. Read-only TOML substring scan — Codex
// owns the write (via `codex mcp add`), so dex never needs a TOML parser (#844).
func codexMCPLocation() string {
	path := codexConfigPath()
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if codexDexConfigured(raw) {
		if strings.HasPrefix(path, os.Getenv("HOME")) && os.Getenv("HOME") != "" {
			return "~/.codex/config.toml"
		}
		return path
	}
	return ""
}

// codexDexConfigured reports whether raw declares an [mcp_servers.dex] table
// whose command is dex with a `mcp` arg.
func codexDexConfigured(raw []byte) bool {
	sec := codexDexSection(string(raw))
	return sec != "" && strings.Contains(sec, `"dex"`) && strings.Contains(sec, `"mcp"`)
}

// codexDexSection returns the [mcp_servers.dex] table together with its child
// tables (e.g. [mcp_servers.dex.env]), stopping at the next unrelated table
// header. Returns "" when the dex server is absent. Read-only line scan — Codex
// owns the TOML write, so dex needs no TOML parser (#844).
func codexDexSection(s string) string {
	const head = "[mcp_servers.dex]"
	start := strings.Index(s, head)
	if start == -1 {
		return ""
	}
	lines := strings.Split(s[start:], "\n")
	var b strings.Builder
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		// A new table header that is neither the dex table nor one of its
		// children (e.g. .env) ends the section.
		if i > 0 && strings.HasPrefix(trimmed, "[") &&
			trimmed != head && !strings.HasPrefix(trimmed, "[mcp_servers.dex.") {
			break
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// codexEnvDrift returns core vars whose live value differs from the snapshot
// recorded in Codex's config.toml (set-but-different, or set-now-but-absent).
func codexEnvDrift() []string {
	path := codexConfigPath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := codexDexSection(string(raw))
	var stale []string
	for _, k := range codexCoreVars {
		live, set := os.LookupEnv(k)
		if !set || live == "" {
			continue
		}
		if !codexEnvLineInSync(body, k, live) {
			stale = append(stale, k)
		}
	}
	return stale
}

// codexEnvLineInSync reports whether the table body has a `KEY = "live"` line
// (TOML keys are bare, values quoted). A KEY line whose value differs from the
// live env — or no KEY line at all — counts as out of sync.
func codexEnvLineInSync(body, key, live string) bool {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, key) {
			continue
		}
		rest := strings.TrimSpace(t[len(key):])
		if !strings.HasPrefix(rest, "=") {
			continue // e.g. KEY_SUFFIX — not our key
		}
		return strings.Contains(rest, `"`+live+`"`)
	}
	return false
}

// dexPluginManifest reports whether raw is a .claude-plugin/manifest.json
