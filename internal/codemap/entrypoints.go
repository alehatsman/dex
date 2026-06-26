package codemap

import (
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// DefaultEntrypointsBudget caps the orientation "entrypoints" section (#581).
// Small — a repo has a handful of main() functions at most.
const DefaultEntrypointsBudget = 120

const entrypointsHeader = "## entrypoints\n"

// RenderEntrypoints renders the orientation "entrypoints" section: where the
// project's execution starts (its main() functions), one per line, greedily fit
// to budget. Returns "" for a library with no main — the section is then
// omitted, leaving the bundle byte-identical. paths must be pre-sorted by the
// caller for a cache-stable render.
func RenderEntrypoints(paths []string, budget int) string {
	if len(paths) == 0 {
		return ""
	}
	if budget <= 0 {
		budget = DefaultEntrypointsBudget
	}
	var b strings.Builder
	b.WriteString(entrypointsHeader)
	for _, p := range paths {
		line := "- main — " + p + "\n"
		if b.Len() > len(entrypointsHeader) && tokens.Count(b.String()+line) > budget {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
