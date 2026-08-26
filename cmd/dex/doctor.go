// `dex doctor` — one-shot setup diagnostic.
//
// Runs all configured checks (index dir, endpoints, project config, MCP
// wiring) and prints a labelled pass/fail table with actionable fix hints.
// Exits 1 when any critical check fails (embed unreachable, index dir
// missing/unwritable).
//
// The backend-readiness diagnosis (endpoint liveness + --deep capability
// probes, and the check/status vocabulary) lives in internal/health; this file
// keeps flag parsing, the non-backend checks, and terminal rendering.
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

	"github.com/alehatsman/dex/internal/gitworktree"
	"github.com/alehatsman/dex/internal/health"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/mcp"
)

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	setHelp(fs,
		"Check the dex setup: index dir, endpoints, project config, and MCP wiring.",
		"dex doctor",
		"dex doctor",
		"dex doctor --deep",
	)
	verbose := fs.Bool("v", false, "verbose: accepted for consistency (endpoint details always shown)")
	deep := fs.Bool("deep", false, "deep readiness: send one minimal real request per configured backend (may load models; slower)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("doctor takes no arguments")
	}
	_ = verbose

	fmt.Printf("dex doctor  (%s)\n\n", mcp.Version)

	epCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var checks []health.Check
	checks = append(checks, checkIndexDir())
	checks = append(checks, health.CheckEndpoints(epCtx, collectEndpoints())...)
	if *deep {
		// Deep probes cold-load models, so they get their own longer budget
		// separate from the 5s liveness context.
		deepCtx, dcancel := context.WithTimeout(ctx, 60*time.Second)
		checks = append(checks, health.CheckEndpointsDeep(deepCtx, collectEndpoints())...)
		dcancel()
	}
	checks = append(checks, checkProjectConfig())
	checks = append(checks, checkMCPWiring())
	checks = append(checks, checkRulesWiring())

	labelW := 0
	for _, c := range checks {
		if len(c.Name) > labelW {
			labelW = len(c.Name)
		}
	}

	var issues, critFails int
	for _, c := range checks {
		fmt.Printf("  %-*s  %s  %s\n", labelW, c.Name, docSym(c.Status), c.Detail)
		for _, h := range c.Hints {
			fmt.Printf("  %-*s     →  %s\n", labelW, "", h)
		}
		if c.Status == health.Fail || c.Status == health.Warn {
			issues++
		}
		if c.Status == health.Fail && c.Critical {
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

func docSym(s health.Status) string {
	switch s {
	case health.OK:
		return "✓"
	case health.Warn:
		return "⚠"
	case health.Fail:
		return "✗"
	default:
		return "—"
	}
}

// ─── index dir ────────────────────────────────────────────────────────────────

func checkIndexDir() health.Check {
	base, err := indexDir()
	if err != nil {
		return health.Check{Name: "index dir", Status: health.Fail, Critical: true,
			Detail: "cannot determine index dir: " + err.Error()}
	}

	fi, err := os.Stat(base)
	if err != nil {
		if !os.IsNotExist(err) {
			return health.Check{Name: "index dir", Status: health.Fail, Critical: true,
				Detail: fmt.Sprintf("%s: %v", base, err)}
		}
		if mkErr := os.MkdirAll(base, 0o755); mkErr != nil {
			return health.Check{
				Name: "index dir", Status: health.Fail, Critical: true,
				Detail: "does not exist and cannot be created: " + base,
				Hints:  []string{"mkdir -p " + base},
			}
		}
		return health.Check{Name: "index dir", Status: health.OK, Detail: base + "  (created)"}
	}
	if !fi.IsDir() {
		return health.Check{Name: "index dir", Status: health.Fail, Critical: true,
			Detail: base + " exists but is not a directory"}
	}

	tmp, werr := os.CreateTemp(base, ".dex-doctor-")
	if werr != nil {
		return health.Check{
			Name: "index dir", Status: health.Fail, Critical: true,
			Detail: fmt.Sprintf("%s is not writable: %v", base, werr),
			Hints:  []string{"chmod u+w " + base},
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
		return health.Check{
			Name:   "index dir",
			Status: health.Warn,
			Detail: detail,
			Hints:  []string{"run: dex index <path> to index a project"},
		}
	}
	return health.Check{Name: "index dir", Status: health.OK, Detail: detail}
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

// ─── project config ───────────────────────────────────────────────────────────

func checkProjectConfig() health.Check {
	wd, err := os.Getwd()
	if err != nil {
		return health.Check{Name: "project cfg", Status: health.Warn,
			Detail: "cannot determine cwd: " + err.Error()}
	}

	cfgPath := filepath.Join(wd, ".dex", "config.yml")
	// #108: a linked worktree with no config of its own inherits the main
	// working tree's — report that instead of "nothing will be indexed", which
	// would now contradict the indexer (ignore.New below inherits identically).
	inheritedFrom := ""
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		main, ok := gitworktree.MainWorktree(wd)
		if ok {
			if _, e := os.Stat(filepath.Join(main, ".dex", "config.yml")); e == nil {
				inheritedFrom = main
			}
		}
		if inheritedFrom == "" {
			return health.Check{
				Name:   "project cfg",
				Status: health.Skip,
				Detail: "no .dex/config.yml in " + wd,
				Hints:  []string{"create .dex/config.yml with index.include to enable indexing"},
			}
		}
	}

	ig, err := ignore.New(wd)
	if err != nil {
		return health.Check{Name: "project cfg", Status: health.Warn,
			Detail: ".dex/config.yml parse error: " + err.Error()}
	}
	if !ig.IncludeConfigured() {
		return health.Check{
			Name:   "project cfg",
			Status: health.Warn,
			Detail: ".dex/config.yml present but no index.include — nothing will be indexed",
			Hints:  []string{"add index.include to .dex/config.yml"},
		}
	}
	if inheritedFrom != "" {
		return health.Check{Name: "project cfg", Status: health.OK,
			Detail: ".dex/config.yml inherited from " + inheritedFrom + " (worktree)"}
	}
	return health.Check{Name: "project cfg", Status: health.OK, Detail: ".dex/config.yml  " + wd}
}

// ─── routing rules ────────────────────────────────────────────────────────────

func checkRulesWiring() health.Check {
	st, path := checkRulesStatus()
	switch st {
	case rulesInSync:
		return health.Check{Name: "rules", Status: health.OK, Detail: "up to date  (" + path + ")"}
	case rulesMissing:
		return health.Check{
			Name: "rules", Status: health.Warn,
			Detail: "routing rules file not found",
			Hints:  []string{"run: dex setup"},
		}
	case rulesNoMarkers:
		return health.Check{
			Name: "rules", Status: health.Warn,
			Detail: "dex block missing in " + path,
			Hints:  []string{"run: dex setup to inject routing rules"},
		}
	case rulesStale:
		return health.Check{
			Name: "rules", Status: health.Warn,
			Detail: "stale version in " + path,
			Hints:  []string{"run: dex setup to update"},
		}
	case rulesDrifted:
		return health.Check{
			Name: "rules", Status: health.Warn,
			Detail: "content drifted from canonical in " + path,
			Hints:  []string{"run: dex setup to restore"},
		}
	}
	return health.Check{Name: "rules", Status: health.OK, Detail: path}
}

// ─── MCP wiring ───────────────────────────────────────────────────────────────

func checkMCPWiring() health.Check {
	// Check multiple locations where dex MCP can be registered.
	if where := dexMCPLocation(); where != "" {
		return health.Check{Name: "mcp", Status: health.OK, Detail: "configured (" + where + ")"}
	}

	return health.Check{
		Name:   "mcp",
		Status: health.Warn,
		Detail: "dex not found in Claude Code MCP configuration",
		Hints: []string{
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
