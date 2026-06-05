// `dex doctor` — one-shot setup diagnostic.
//
// Runs all configured checks (index dir, endpoints, project config, MCP
// wiring) and prints a labelled pass/fail table with actionable fix hints.
// Exits 1 when any critical check fails (embed unreachable, index dir
// missing/unwritable).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/mcp"
)

// doctorStatus classifies the outcome of one check.
type doctorStatus int

const (
	docOK   doctorStatus = iota // ✓
	docWarn                     // ⚠ non-critical problem
	docFail                     // ✗ critical failure
	docSkip                     // — not configured / not applicable
)

type doctorCheck struct {
	name     string
	status   doctorStatus
	detail   string
	hints    []string // printed as "→ <hint>" under the row
	critical bool     // docFail on a critical check → exit 1
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	setHelp(fs,
		"Check the dex setup: index dir, endpoints, project config, and MCP wiring.",
		"dex doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("doctor takes no arguments")
	}

	fmt.Printf("dex doctor  (%s)\n\n", mcp.Version)

	epCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var checks []doctorCheck
	checks = append(checks, checkIndexDir())
	checks = append(checks, checkEndpoints(epCtx)...)
	checks = append(checks, checkProjectConfig())
	checks = append(checks, checkMCPWiring())

	labelW := 0
	for _, c := range checks {
		if len(c.name) > labelW {
			labelW = len(c.name)
		}
	}

	var issues, critFails int
	for _, c := range checks {
		fmt.Printf("  %-*s  %s  %s\n", labelW, c.name, docSym(c.status), c.detail)
		for _, h := range c.hints {
			fmt.Printf("  %-*s     →  %s\n", labelW, "", h)
		}
		if c.status == docFail || c.status == docWarn {
			issues++
		}
		if c.status == docFail && c.critical {
			critFails++
		}
	}

	fmt.Println()
	if issues == 0 {
		fmt.Println("all checks passed")
		return nil
	}
	fmt.Printf("%d issue(s) found\n", issues)
	if critFails > 0 {
		return fmt.Errorf("%d critical check(s) failed", critFails)
	}
	return nil
}

func docSym(s doctorStatus) string {
	switch s {
	case docOK:
		return "✓"
	case docWarn:
		return "⚠"
	case docFail:
		return "✗"
	default:
		return "—"
	}
}

// ─── index dir ────────────────────────────────────────────────────────────────

func checkIndexDir() doctorCheck {
	base, err := indexDir()
	if err != nil {
		return doctorCheck{name: "index dir", status: docFail, critical: true,
			detail: "cannot determine index dir: " + err.Error()}
	}

	fi, err := os.Stat(base)
	if err != nil {
		if !os.IsNotExist(err) {
			return doctorCheck{name: "index dir", status: docFail, critical: true,
				detail: fmt.Sprintf("%s: %v", base, err)}
		}
		if mkErr := os.MkdirAll(base, 0o755); mkErr != nil {
			return doctorCheck{
				name: "index dir", status: docFail, critical: true,
				detail: "does not exist and cannot be created: " + base,
				hints:  []string{"mkdir -p " + base},
			}
		}
		return doctorCheck{name: "index dir", status: docOK, detail: base + "  (created)"}
	}
	if !fi.IsDir() {
		return doctorCheck{name: "index dir", status: docFail, critical: true,
			detail: base + " exists but is not a directory"}
	}

	tmp, werr := os.CreateTemp(base, ".dex-doctor-")
	if werr != nil {
		return doctorCheck{
			name: "index dir", status: docFail, critical: true,
			detail: fmt.Sprintf("%s is not writable: %v", base, werr),
			hints:  []string{"chmod u+w " + base},
		}
	}
	_ = os.Remove(tmp.Name())
	_ = tmp.Close()

	n := countIndexedProjects(base)
	noun := "projects"
	if n == 1 {
		noun = "project"
	}
	detail := fmt.Sprintf("%s  (%d %s)", base, n, noun)
	if n == 0 {
		return doctorCheck{
			name:   "index dir",
			status: docWarn,
			detail: detail,
			hints:  []string{"run: dex index <path> to index a project"},
		}
	}
	return doctorCheck{name: "index dir", status: docOK, detail: detail}
}

func countIndexedProjects(base string) int {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(base, e.Name(), "index.db")); err == nil {
				n++
			}
		}
	}
	return n
}

// ─── endpoints ────────────────────────────────────────────────────────────────

type probeResult struct {
	name string
	err  error
}

func checkEndpoints(ctx context.Context) []doctorCheck {
	probes := collectEndpoints()

	ch := make(chan probeResult, len(probes))
	launched := 0
	for i := range probes {
		if probes[i].health == nil {
			continue
		}
		p := &probes[i]
		launched++
		go func() { ch <- probeResult{p.name, p.health(ctx)} }()
	}

	errs := make(map[string]error, launched)
	for range launched {
		r := <-ch
		errs[r.name] = r.err
	}

	out := make([]doctorCheck, 0, len(probes))
	for _, p := range probes {
		out = append(out, endpointCheck(p, errs[p.name]))
	}
	return out
}

func endpointCheck(p endpointProbe, err error) doctorCheck {
	if p.health == nil {
		detail := p.status
		if detail == "inherits chat" {
			detail = "inherits chat endpoint"
		}
		return doctorCheck{name: p.name, status: docSkip, detail: detail}
	}

	if err != nil {
		status := docWarn
		critical := false
		if p.name == "embed" {
			status = docFail
			critical = true
		}
		return doctorCheck{
			name:     p.name,
			status:   status,
			critical: critical,
			detail:   fmt.Sprintf("UNREACHABLE  (%s)", p.url),
			hints:    endpointHints(p.name, p.url),
		}
	}

	detail := p.url
	if p.model != "" {
		detail = p.url + "  " + p.model
	}
	return doctorCheck{name: p.name, status: docOK, detail: detail}
}

func endpointHints(name, url string) []string {
	switch name {
	case "embed":
		if scan, ok := embed.ScanOllama(context.Background()); ok {
			if len(scan.EmbedModels) == 0 {
				return []string{
					fmt.Sprintf("ollama is running but has no embed model — run: ollama pull %s", embed.DefaultPullModel),
					"or: dex reindex --pull-model <path>",
				}
			}
			return []string{"check that the embedding service is running at " + url}
		}
		return []string{
			"start ollama: ollama serve",
			fmt.Sprintf("then: ollama pull %s", embed.DefaultPullModel),
			"or set DEX_EMBED_URL to a running OpenAI-compatible embedding service",
		}
	case "chat":
		return []string{
			"start ollama (auto-detected) or set DEX_CHAT_URL",
			"chat is required for: dex ask, dex generate, dex view summarize",
		}
	}
	return nil
}

// ─── project config ───────────────────────────────────────────────────────────

func checkProjectConfig() doctorCheck {
	wd, err := os.Getwd()
	if err != nil {
		return doctorCheck{name: "project cfg", status: docWarn,
			detail: "cannot determine cwd: " + err.Error()}
	}

	cfgPath := filepath.Join(wd, ".dex", "config.yml")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return doctorCheck{
			name:   "project cfg",
			status: docSkip,
			detail: "no .dex/config.yml in " + wd,
			hints:  []string{"create .dex/config.yml with index.include to enable indexing"},
		}
	}

	ig, err := ignore.New(wd)
	if err != nil {
		return doctorCheck{name: "project cfg", status: docWarn,
			detail: ".dex/config.yml parse error: " + err.Error()}
	}
	if !ig.IncludeConfigured() {
		return doctorCheck{
			name:   "project cfg",
			status: docWarn,
			detail: ".dex/config.yml present but no index.include — nothing will be indexed",
			hints:  []string{"add index.include to .dex/config.yml"},
		}
	}
	return doctorCheck{name: "project cfg", status: docOK, detail: ".dex/config.yml  " + wd}
}

// ─── MCP wiring ───────────────────────────────────────────────────────────────

func checkMCPWiring() doctorCheck {
	// Check multiple locations where dex MCP can be registered.
	if where := dexMCPLocation(); where != "" {
		return doctorCheck{name: "mcp", status: docOK, detail: "configured (" + where + ")"}
	}

	return doctorCheck{
		name:   "mcp",
		status: docWarn,
		detail: "dex not found in Claude Code MCP configuration",
		hints: []string{
			"run: claude mcp add --scope user dex -- dex mcp",
			"or install via the plugin: dex has a .claude-plugin/manifest.json",
		},
	}
}

// dexMCPLocation returns a short description of where dex MCP is
// configured, or "" if it is not found in any of the standard locations.
func dexMCPLocation() string {
	home, _ := os.UserHomeDir()

	if home != "" {
		// 1. ~/.claude.json — primary storage for `claude mcp add --scope user`.
		//    Claude Code writes mcpServers here, NOT to settings.json.
		if raw, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
			if dexMCPConfigured(raw) {
				return "~/.claude.json"
			}
		}

		// 2. ~/.claude/settings.json — kept for completeness; mcpServers is not
		//    a valid settings.json field but some tooling may write it there.
		if raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
			if dexMCPConfigured(raw) {
				return "~/.claude/settings.json"
			}
		}
	}

	// 3. .claude/settings.json in cwd (project-level)
	if wd, err := os.Getwd(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(wd, ".claude", "settings.json")); err == nil {
			if dexMCPConfigured(raw) {
				return ".claude/settings.json"
			}
		}
		// 4. .claude-plugin/manifest.json in cwd (plugin manifest)
		if raw, err := os.ReadFile(filepath.Join(wd, ".claude-plugin", "manifest.json")); err == nil {
			if dexPluginManifest(raw) {
				return ".claude-plugin/manifest.json"
			}
		}
	}
	return ""
}

// dexMCPConfigured reports whether raw contains an mcpServers entry whose
// command is `dex` and whose args include "mcp". Falls back to a raw
// substring scan when JSON doesn't match the expected shape.
func dexMCPConfigured(raw []byte) bool {
	var settings struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &settings) == nil {
		for _, srv := range settings.MCPServers {
			if filepath.Base(srv.Command) == "dex" {
				for _, a := range srv.Args {
					if a == "mcp" {
						return true
					}
				}
			}
		}
		if len(settings.MCPServers) > 0 {
			return false // parsed OK but no matching entry
		}
	}
	// Fallback: raw scan for common patterns.
	s := string(raw)
	return strings.Contains(s, `"dex"`) && strings.Contains(s, `"mcp"`)
}

// dexPluginManifest reports whether raw is a .claude-plugin/manifest.json
// that wires dex as an MCP server.
func dexPluginManifest(raw []byte) bool {
	var manifest struct {
		MCP struct {
			Command string `json:"command"`
		} `json:"mcp"`
	}
	if json.Unmarshal(raw, &manifest) == nil && filepath.Base(manifest.MCP.Command) == "dex" {
		return true
	}
	s := string(raw)
	return strings.Contains(s, `"mcp"`) && strings.Contains(s, `"dex"`)
}
