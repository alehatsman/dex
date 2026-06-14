// `dex completion` — emit shell tab-completion scripts.
//
// Usage:
//
//	dex completion bash    # bash (requires bash-completion)
//	dex completion zsh     # zsh
//	dex completion fish    # fish
//
// The scripts are GENERATED from the canonical verb registry (registry.go) by
// the renderers in completion_gen.go — top-level command list, per-command
// flags, choices, and subcommands all come from one place, so the three shells
// cannot drift out of sync (#469, #482).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func cmdCompletion(args []string) error {
	fs := flag.NewFlagSet("completion", flag.ContinueOnError)
	setHelp(fs,
		"Output shell tab-completion script for dex.",
		"dex completion bash|zsh|fish",
		`dex completion zsh > /tmp/dex-comp && source /tmp/dex-comp`,
		`source <(dex completion bash)  # add to ~/.bashrc`,
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("completion needs a shell: bash | zsh | fish")
	}
	switch fs.Arg(0) {
	case "bash":
		fmt.Print(bashCompletionScript())
		fmt.Fprintln(os.Stderr, "# to install, add to ~/.bashrc:\n#   source <(dex completion bash)")
	case "zsh":
		fmt.Print(zshCompletionScript())
		fmt.Fprintln(os.Stderr, "# to install, add to ~/.zshrc:\n#   source <(dex completion zsh)")
	case "fish":
		fmt.Print(fishCompletionScript())
		fmt.Fprintln(os.Stderr, "# to install:\n#   dex completion fish > ~/.config/fish/completions/dex.fish")
	default:
		return fmt.Errorf("unknown shell %q (want bash | zsh | fish)", fs.Arg(0))
	}
	return nil
}

// bashCompletionScript renders the bash script, injecting the command list,
// subcommand cases, and flag completions generated from the registry.
func bashCompletionScript() string {
	s := bashCompletionTemplate
	s = strings.ReplaceAll(s, "__DEX_TOP_COMMANDS__", strings.Join(completionCommands(), " "))
	s = strings.ReplaceAll(s, "__DEX_BASH_SUBCMDS__", bashSubcmdCases())
	s = strings.ReplaceAll(s, "__DEX_BASH_FLAGVALS__", bashFlagValueCases())
	s = strings.ReplaceAll(s, "__DEX_BASH_FLAGNAMES__", bashFlagNameCases())
	return s
}

const bashCompletionTemplate = `# dex bash completion
# source <(dex completion bash)

_dex_completion() {
    local cur prev words cword
    _init_completion 2>/dev/null || {
        COMPREPLY=()
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
    }

    local top_commands="__DEX_TOP_COMMANDS__"

    # Depth-1: top-level command
    if [[ $COMP_CWORD -eq 1 ]]; then
        COMPREPLY=($(compgen -W "$top_commands" -- "$cur"))
        return
    fi

    local cmd="${COMP_WORDS[1]}"

    # Depth-2: subcommands
    if [[ $COMP_CWORD -eq 2 ]]; then
        case $cmd in
__DEX_BASH_SUBCMDS__
        esac
    fi

    # Flag-value completion (command-scoped: --flag val and --flag=val)
    case $cmd in
__DEX_BASH_FLAGVALS__
    esac

    # Flag-name completion
    if [[ $cur == -* ]]; then
        case $cmd in
__DEX_BASH_FLAGNAMES__
        esac
    fi

    # Path arg: directory completion
    COMPREPLY=($(compgen -d -- "$cur"))
}
complete -F _dex_completion dex
`

// zshCompletionScript renders the zsh script, injecting the command list (with
// per-command descriptions) and the per-command `_arguments` blocks generated
// from the registry.
func zshCompletionScript() string {
	lines := make([]string, 0, len(verbs))
	for _, entry := range zshCommandList() {
		lines = append(lines, "        '"+entry+"'")
	}
	s := zshCompletionTemplate
	s = strings.ReplaceAll(s, "__DEX_TOP_COMMANDS_ZSH__", strings.Join(lines, "\n"))
	s = strings.ReplaceAll(s, "__DEX_ZSH_ARG_BLOCKS__", zshArgBlocks())
	return s
}

const zshCompletionTemplate = `#compdef dex
# dex zsh completion
# source <(dex completion zsh)

_dex() {
    local context state line
    typeset -A opt_args

    local -a top_commands
    top_commands=(
__DEX_TOP_COMMANDS_ZSH__
    )

    _arguments -C \
        '(-h --help)'{-h,--help}'[show help]' \
        '1: :->command' \
        '*:: :->args' && return 0

    case $state in
        command)
            _describe 'command' top_commands
            ;;
        args)
            case $words[1] in
__DEX_ZSH_ARG_BLOCKS__
                *)
                    _files -/
                    ;;
            esac
            ;;
    esac
}

# Register the completer. Works both when this file is sourced
# (source <(dex completion zsh)) and when it is dropped into $fpath as a
# #compdef autoload file — in the latter case zsh invokes _dex directly, so
# funcstack[1] is _dex and we run it; otherwise we bind it with compdef.
if [ "$funcstack[1]" = "_dex" ]; then
    _dex
else
    compdef _dex dex
fi
`

// fishCompletionScript renders the fish script, injecting the command list,
// subcommand completions, and per-flag completions generated from the registry.
func fishCompletionScript() string {
	s := fishCompletionTemplate
	s = strings.ReplaceAll(s, "__DEX_TOP_COMMANDS__", strings.Join(completionCommands(), " "))
	s = strings.ReplaceAll(s, "__DEX_FISH_TOPCMDS__", fishTopCommands())
	s = strings.ReplaceAll(s, "__DEX_FISH_SUBCMDS__", fishSubcmdCompletions())
	s = strings.ReplaceAll(s, "__DEX_FISH_FLAGS__", fishFlagCompletions())
	return s
}

const fishCompletionTemplate = `# dex fish completions
# dex completion fish > ~/.config/fish/completions/dex.fish

set -l top_cmds __DEX_TOP_COMMANDS__

# Top-level commands (with descriptions)
__DEX_FISH_TOPCMDS__

__DEX_FISH_SUBCMDS__

__DEX_FISH_FLAGS__
`
