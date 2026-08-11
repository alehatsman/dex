package codemap

import (
	"strings"

	"github.com/alehatsman/dex/internal/tokens"
)

// DefaultCommandsBudget caps the orientation "commands" section — a handful of
// build/test/run lines at most.
const DefaultCommandsBudget = 160

const commandsHeader = "## commands\n"

// Command is one operational command an agent can run to build, test, lint, or
// run the repo, paired with the role it fills (its Label).
type Command struct {
	Label string // role: build, test, lint, run, ci
	Cmd   string // the concrete shell command
}

// RenderCommands renders the orientation "commands" section: how to build, test,
// lint, and run the repo, one per line, greedily fit to budget. Returns "" when
// no commands were found (a repo with no task runner) — the section is then
// omitted, leaving the bundle byte-identical. cmds must be pre-sorted by the
// caller for a cache-stable render.
func RenderCommands(cmds []Command, budget int) string {
	if len(cmds) == 0 {
		return ""
	}
	if budget <= 0 {
		budget = DefaultCommandsBudget
	}
	var b strings.Builder
	b.WriteString(commandsHeader)
	for _, c := range cmds {
		line := "- " + c.Label + ": `" + c.Cmd + "`\n"
		if b.Len() > len(commandsHeader) && tokens.Count(b.String()+line) > budget {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
