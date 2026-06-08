// `dex setup` — write Claude Code routing rules to ~/.claude/rules/dex.md.
//
// This makes dex tools the default instead of native equivalents by injecting
// a versioned rules block that Claude Code auto-loads from $CLAUDE_CONFIG_DIR/rules/.
// Safe to run multiple times — idempotent when already up to date, upgrades a
// stale block, appends to an existing file that predates dex.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	rulesMarker    = "# dex — semantic search & context routing"
	rulesEndMarker = "<!-- /dex -->"
	rulesVersion   = "<!-- dex-rules-v1 -->"
)

const rulesContent = `# dex — semantic search & context routing
<!-- dex-rules-v1 -->

## Tool Mapping (prefer dex tools over native equivalents)
| Instead of | Use | When |
|------------|-----|------|
| Grep / rg (concept search) | ` + "`search_semantic(q, path)`" + ` | intent / keyword searches |
| Grep / rg (exact name) | ` + "`search_symbol(name, path)`" + ` | exact identifier lookup |
| Read (large files >400 lines) | ` + "`file_view(file)`" + ` | signatures + summaries view |
| Bash (build/test output) | ` + "`ctx_shell(command)`" + ` | compressed shell output |
| Manual cross-ref tracing | ` + "`graph_callers / graph_callees`" + ` | call-graph navigation |
| Manual import scanning | ` + "`graph_deps(path)`" + ` | dependency edges |

## Workflow
1. **Orient:** ` + "`ask(question)`" + ` — routes intent, returns suggested_reads + next_action
2. **Locate:** ` + "`search_semantic`" + ` for concepts; ` + "`search_symbol`" + ` for exact names
3. **Read:** ` + "`file_view`" + ` for large files; native Read for small ones (<400 lines)
4. **Shell:** ` + "`ctx_shell(command)`" + ` for build/test/grep output

## Proactive (call without being asked)
- ` + "`ask(task)`" + ` at the start of every session to orient on the codebase
<!-- /dex -->`

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print what would be written without modifying any files")
	setHelp(fs,
		"Write dex routing rules to $CLAUDE_CONFIG_DIR/rules/dex.md.\n"+
			"Claude Code auto-loads all files from that directory at session start,\n"+
			"so dex tools become the default without any further configuration.",
		"dex setup [--dry-run]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("setup takes no arguments")
	}

	rulesPath, err := claudeRulesPath()
	if err != nil {
		return err
	}

	action, newContent, err := buildRulesContent(rulesPath)
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Printf("would write %s (%s)\n\n%s\n", rulesPath, action, newContent)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(rulesPath), 0o755); err != nil {
		return fmt.Errorf("create rules dir: %w", err)
	}
	if err := os.WriteFile(rulesPath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write rules: %w", err)
	}

	fmt.Printf("%s  %s\n", action, rulesPath)
	return nil
}

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

// buildRulesContent reads the existing file (if any) and returns:
//   - action: "created", "updated", or "already up to date"
//   - the full new file content
func buildRulesContent(path string) (action, content string, err error) {
	existing, readErr := os.ReadFile(path)
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", "", fmt.Errorf("read %s: %w", path, readErr)
		}
		// New file — write rules only.
		return "created", rulesContent + "\n", nil
	}

	s := string(existing)

	// Already current.
	if strings.Contains(s, rulesVersion) {
		return "already up to date", s, nil
	}

	// Stale dex block — replace the marked section.
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

	// File exists but no dex block — append.
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
