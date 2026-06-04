package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/mcp"
)

func cmdCompressStdin(args []string) error {
	fs := flag.NewFlagSet("compress-stdin", flag.ContinueOnError)
	command := fs.String("command", "", "command hint (e.g. 'go test', 'git diff') — selects compression patterns")
	maxLines := fs.Int("max-lines", 200, "hard cap on output lines")
	raw := fs.Bool("raw", false, "passthrough — bypass compression")
	if err := fs.Parse(args); err != nil {
		return err
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if *raw {
		_, err = os.Stdout.Write(input)
		return err
	}

	compressed, _, _ := mcp.CompressText(string(input), *command, *maxLines)
	_, err = fmt.Fprint(os.Stdout, compressed)
	return err
}

// shellHookScript is the eval-able bash/zsh hook emitted by `dex shell-hook`.
// It wraps common commands so their output is piped through `dex compress-stdin`,
// reducing token cost when an agent captures command output. Requires dex to be
// on PATH. The user adds `eval "$(dex shell-hook)"` to their shell profile.
const shellHookScript = `# dex shell hook — pipe high-volume commands through dex compress-stdin.
# Add to ~/.bashrc or ~/.zshrc:  eval "$(dex shell-hook)"
_dex_run() {
  local cmd="$1"
  shift
  command "$cmd" "$@" 2>&1 | dex compress-stdin --command "$cmd"
}
alias git='_dex_run git'
alias go='_dex_run go'
alias cargo='_dex_run cargo'
alias npm='_dex_run npm'
alias yarn='_dex_run yarn'
alias bun='_dex_run bun'
alias pnpm='_dex_run pnpm'
alias docker='_dex_run docker'
`

func cmdShellHook(_ []string) error {
	_, err := fmt.Fprint(os.Stdout, strings.TrimLeft(shellHookScript, "\n"))
	return err
}
