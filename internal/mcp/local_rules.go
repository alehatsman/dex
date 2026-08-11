package mcp

import (
	"os"
	"path/filepath"
	"strings"
)

// collectLocalRules scans the project root for rule/spec files (CLAUDE.md,
// .dex/rules.md, docs/*.md, specs/*.md) — the constraints that govern edits to
// the working set. Surfaced by ask(intent=assemble) as ContextOutput.Rules so a
// task-start pack carries its governing rules (#141, formerly brief.LocalRules).
func collectLocalRules(root string) []string {
	var rules []string

	// Check known root-level rule files.
	candidates := []string{
		"CLAUDE.md",
		".dex/rules.md",
	}
	for _, name := range candidates {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			rules = append(rules, name)
		}
	}

	// Check docs/*.md.
	docsDir := filepath.Join(root, "docs")
	if entries, err := os.ReadDir(docsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				rules = append(rules, filepath.Join("docs", e.Name()))
			}
		}
	}

	// Check specs/*.md.
	specsDir := filepath.Join(root, "specs")
	if entries, err := os.ReadDir(specsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				rules = append(rules, filepath.Join("specs", e.Name()))
			}
		}
	}

	return rules
}
