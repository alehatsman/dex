// `dex setup` — guided first-run wizard.
//
// Walks a new user through: diagnose endpoints, offer to index the cwd,
// surface MCP wiring commands, write Claude Code routing rules, and show a
// working example. Idempotent and re-runnable. Non-interactive (CI/dotfiles):
// `dex setup --check`.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/health"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

func cmdSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	setHelp(fs,
		"Guided first-run wizard: check setup, optionally index the cwd, show MCP wiring.",
		"dex setup [--check]",
		"dex setup          # interactive walkthrough",
		"dex setup --check  # CI: exit 0 if fully set up, 1 otherwise",
	)
	checkOnly := fs.Bool("check", false, "non-interactive: exit 0 if setup is complete, 1 otherwise")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("setup takes no arguments")
	}

	fmt.Printf("dex setup  (%s)\n\n", mcp.Version)

	// ── step 1: run doctor checks ────────────────────────────────────────
	epCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	checks := []health.Check{
		checkIndexDir(),
	}
	checks = append(checks, health.CheckEndpoints(epCtx, collectEndpoints())...)
	checks = append(checks, checkProjectConfig())
	checks = append(checks, checkMCPWiring())

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

	// ── check-only mode ───────────────────────────────────────────────────
	if *checkOnly {
		if critFails > 0 {
			fmt.Fprintf(os.Stderr, "setup incomplete: %d critical issue(s)\n", critFails)
			os.Exit(1)
		}
		if issues > 0 {
			fmt.Fprintf(os.Stderr, "setup incomplete: %d issue(s)\n", issues)
			os.Exit(1)
		}
		fmt.Println("setup complete")
		return nil
	}

	if critFails > 0 {
		fmt.Printf("⚠ %d critical issue(s) — fix endpoints before continuing\n", critFails)
		fmt.Println("  run `dex doctor` for details and hints")
		fmt.Println()
		fmt.Println("next: `dex help` for common commands")
		return nil
	}

	// ── step 2: check whether cwd is indexed ─────────────────────────────
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	base, err := indexDir()
	if err != nil {
		return err
	}
	if p, perr := proj.Resolve(wd, base); perr == nil {
		if _, serr := os.Stat(p.DBPath); os.IsNotExist(serr) {
			fmt.Printf("current project is not indexed: %s\n", wd)
			// Ensure a config with an active include exists so the index step
			// below (or a later `dex index .`) actually indexes something (#546).
			if cfgPath, created, cerr := scaffoldConfigIfAbsent(wd); cerr != nil {
				fmt.Fprintf(os.Stderr, "  could not scaffold config: %v\n", cerr)
			} else if created {
				fmt.Printf("  scaffolded %s (indexes the whole tree by default — edit to scope)\n", cfgPath)
			}
			if stdinIsTTY() {
				fmt.Fprintf(os.Stderr, "  run `dex index .` now? [y/N] ")
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				if ans := strings.TrimSpace(strings.ToLower(line)); ans == "y" || ans == "yes" {
					fmt.Println()
					if ixErr := cmdIndex(ctx, []string{wd}); ixErr != nil {
						fmt.Fprintf(os.Stderr, "  index failed: %v\n  fix the issue and re-run `dex index .`\n", ixErr)
					}
				} else {
					fmt.Println("  skipped — run: dex index .")
				}
			} else {
				fmt.Println("  run: dex index .")
			}
			fmt.Println()
		} else {
			fmt.Printf("✓ %s is indexed\n\n", wd)
		}
	}

	// ── step 3: MCP wiring ────────────────────────────────────────────────
	if dexMCPLocation() == "" {
		fmt.Println("MCP is not wired to Claude Code. To register dex:")
		fmt.Println("  claude mcp add --scope user dex -- dex mcp")
		fmt.Println()
	} else {
		fmt.Printf("✓ MCP configured (%s)\n\n", dexMCPLocation())
	}

	// ── step 4: write Claude Code routing rules ───────────────────────────
	if rulesPath, pathErr := claudeRulesPath(); pathErr == nil {
		action, newContent, buildErr := buildRulesContent(rulesPath)
		if buildErr == nil && action != "already up to date" {
			if mkErr := os.MkdirAll(filepath.Dir(rulesPath), 0o755); mkErr == nil {
				_ = os.WriteFile(rulesPath, []byte(newContent), 0o644)
			}
		}
		if action == "already up to date" {
			fmt.Printf("✓ Claude Code routing rules up to date (%s)\n\n", rulesPath)
		} else {
			fmt.Printf("✓ Claude Code routing rules written (%s)\n\n", rulesPath)
		}
	}

	// ── step 5: show a working example ───────────────────────────────────
	fmt.Println("try it:")
	fmt.Println(`  dex ask . "where is the main entry point?"`)
	fmt.Println()

	// ── step 6: pointer to help ───────────────────────────────────────────
	fmt.Println("next: `dex help` for common commands · `dex help all` for the full reference")

	return nil
}

// ── Claude Code routing rules ─────────────────────────────────────────────

const (
	rulesMarker    = "# dex — semantic search & context routing"
	rulesEndMarker = "<!-- /dex -->"
	rulesVersion   = "<!-- dex-rules-v6 -->"
)

// rulesContent is a deliberately thin pointer block. dex injects the full,
// authoritative tool mapping + workflow live as MCP server instructions
// (generated from the installed binary, so they never drift). Duplicating them
// here only invites the staleness this block used to carry — keep it minimal
// and let the MCP instructions be the single source of truth.
const rulesContent = `# dex — semantic search & context routing
<!-- dex-rules-v6 -->

dex is active as an MCP server, exposing a single read verb — ` + "`query`" + ` — over
the codebase intelligence (advisory/retrieval-only; editing and durable findings
are the harness's job). Its tool mapping and workflow are injected live as
MCP server instructions, generated from the installed binary so they never
drift — prefer those over any static copy of them. Start each session by calling
` + "`query()`" + ` with the task description; its input shape picks the lane (path →
signatures, /regex/ → grep, path:line → slice, symbol → call graph, prose →
semantic pack).
Compose lanes in ONE call with ` + "`|`" + `: ` + "`<seed> | callers|callees|impact | signatures|assemble:N`" + `
runs the stages left-to-right for a whole multi-step walk in one round-trip
(e.g. ` + "`query(\"(*Server).Run | callers | impact\")`" + `). A single segment is
exactly today's single-lane behavior — pipes are additive.
Working in a git worktree? Pass its absolute path as ` + "`project_root`" + ` to every
dex call — the server can't see your shell's cwd and otherwise resolves the
checkout it was started in.
Power lanes (search, trace, deps, clusters, smells, review_diff, …) are gated behind DEX_EXPERT.
<!-- /dex -->`

// claudeRulesPath returns $CLAUDE_CONFIG_DIR/rules/dex.md, falling back to
// ~/.claude/rules/dex.md when the env var is unset.
func claudeRulesPath() (string, error) {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "rules", "dex.md"), nil
}

// ── rules status classification ───────────────────────────────────────────

type rulesStatus int

const (
	rulesInSync    rulesStatus = iota // deployed block matches canonical
	rulesMissing                      // file does not exist
	rulesNoMarkers                    // file exists but no dex block found
	rulesStale                        // block present but version string outdated
	rulesDrifted                      // current version but content differs from canonical
)

// checkRulesStatus classifies the deployed routing rules. Fails open: any
// read or compare error returns rulesInSync so callers stay silent.
func checkRulesStatus() (rulesStatus, string) {
	path, err := claudeRulesPath()
	if err != nil {
		return rulesInSync, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rulesMissing, path
		}
		return rulesInSync, path // fail open
	}
	s := string(data)
	if !strings.Contains(s, rulesMarker) {
		return rulesNoMarkers, path
	}
	if !strings.Contains(s, rulesVersion) {
		return rulesStale, path
	}
	if extractRulesBlock(s) != rulesContent {
		return rulesDrifted, path
	}
	return rulesInSync, path
}

// extractRulesBlock returns the substring from rulesMarker through
// rulesEndMarker (inclusive). Returns "" when either marker is absent.
func extractRulesBlock(s string) string {
	start := strings.Index(s, rulesMarker)
	end := strings.Index(s, rulesEndMarker)
	if start == -1 || end == -1 || end < start {
		return ""
	}
	return s[start : end+len(rulesEndMarker)]
}

// buildRulesContent reads the existing file (if any) and returns:
//   - action: "created", "updated", or "already up to date"
//   - the full new file content
func buildRulesContent(path string) (action, content string, err error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", "", fmt.Errorf("read %s: %w", path, readErr)
		}
		return "created", rulesContent + "\n", nil
	}

	s := string(existing)

	// Already canonical, byte-for-byte? Nothing to do. Compare the same way
	// checkRulesStatus does (extractRulesBlock == rulesContent) so the writer
	// and the drift checker never disagree. Keying off rulesVersion presence
	// here instead would wedge every existing file when rulesContent changes
	// without a version bump: the checker reports drift, but the writer sees
	// the marker and refuses to rewrite.
	if extractRulesBlock(s) == rulesContent {
		return "already up to date", s, nil
	}

	if strings.Contains(s, rulesMarker) {
		start := strings.Index(s, rulesMarker)
		end := strings.Index(s, rulesEndMarker)
		var before, after string
		before = s[:start]
		if end != -1 {
			tail := s[end+len(rulesEndMarker):]
			after = strings.TrimLeft(tail, "\n")
		}
		var b strings.Builder
		b.WriteString(before)
		b.WriteString(rulesContent)
		b.WriteByte('\n')
		if after != "" {
			b.WriteByte('\n')
			b.WriteString(after)
		}
		return "updated", b.String(), nil
	}

	var b strings.Builder
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(rulesContent)
	b.WriteByte('\n')
	return "created", b.String(), nil
}
