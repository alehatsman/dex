// `dex completion` — emit shell tab-completion scripts.
//
// Usage:
//
//	dex completion bash    # bash (requires bash-completion)
//	dex completion zsh     # zsh
//	dex completion fish    # fish
package main

import (
	"flag"
	"fmt"
	"os"
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
		fmt.Print(bashCompletionScript)
		fmt.Fprintln(os.Stderr, "# to install, add to ~/.bashrc:\n#   source <(dex completion bash)")
	case "zsh":
		fmt.Print(zshCompletionScript)
		fmt.Fprintln(os.Stderr, "# to install, add to ~/.zshrc:\n#   source <(dex completion zsh)")
	case "fish":
		fmt.Print(fishCompletionScript)
		fmt.Fprintln(os.Stderr, "# to install:\n#   dex completion fish > ~/.config/fish/completions/dex.fish")
	default:
		return fmt.Errorf("unknown shell %q (want bash | zsh | fish)", fs.Arg(0))
	}
	return nil
}

const bashCompletionScript = `# dex bash completion
# source <(dex completion bash)

_dex_completion() {
    local cur prev words cword
    _init_completion 2>/dev/null || {
        COMPREPLY=()
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
    }

    local top_commands="ask search view graph index generate env compact nuke reindex mcp serve watch clone guide hook compress-stdin shell-hook doctor version completion setup config"

    # Depth-1: top-level command
    if [[ $COMP_CWORD -eq 1 ]]; then
        COMPREPLY=($(compgen -W "$top_commands" -- "$cur"))
        return
    fi

    local cmd="${COMP_WORDS[1]}"

    # Depth-2: subcommands
    if [[ $COMP_CWORD -eq 2 ]]; then
        case $cmd in
            search)     COMPREPLY=($(compgen -W "semantic symbol" -- "$cur")); return ;;
            view)       COMPREPLY=($(compgen -W "summarize" -- "$cur")); return ;;
            graph)      COMPREPLY=($(compgen -W "neighbors deps packages callers callees links backlinks tags export" -- "$cur")); return ;;
            index)      COMPREPLY=($(compgen -W "status summarize" -- "$cur")); return ;;
            hook)       COMPREPLY=($(compgen -W "inject rewrite redirect observe" -- "$cur")); return ;;
            completion) COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur")); return ;;
            config)     COMPREPLY=($(compgen -W "init" -- "$cur")); return ;;
        esac
    fi

    # Flag value completions
    case $prev in
        --intent)  COMPREPLY=($(compgen -W "auto behavior_search symbol_lookup callers callees architecture package_topology editing_context" -- "$cur")); return ;;
        --format)  COMPREPLY=($(compgen -W "text json" -- "$cur")); return ;;
        --graph)   COMPREPLY=($(compgen -W "on off only" -- "$cur")); return ;;
        --rerank)  COMPREPLY=($(compgen -W "off" -- "$cur")); return ;;
    esac

    # --flag=value completions
    case $cur in
        --intent=*) COMPREPLY=($(compgen -W "auto behavior_search symbol_lookup callers callees architecture package_topology editing_context" -- "${cur#*=}")); COMPREPLY=("${COMPREPLY[@]/#/--intent=}"); return ;;
        --format=*) COMPREPLY=($(compgen -W "text json" -- "${cur#*=}")); COMPREPLY=("${COMPREPLY[@]/#/--format=}"); return ;;
        --graph=*)  COMPREPLY=($(compgen -W "on off only" -- "${cur#*=}")); COMPREPLY=("${COMPREPLY[@]/#/--graph=}"); return ;;
        --*)        return ;;
    esac

    # Path arg: directory completion
    COMPREPLY=($(compgen -d -- "$cur"))
}
complete -F _dex_completion dex
`

const zshCompletionScript = `#compdef dex
# dex zsh completion
# source <(dex completion zsh)

_dex() {
    local context state line
    typeset -A opt_args

    local -a top_commands
    top_commands=(
        'ask:one-shot router (semantic + symbol + graph)'
        'search:hybrid top-k or exact symbol search'
        'view:file summarization'
        'graph:graph traversal'
        'index:build or refresh the project index'
        'generate:RAG code generation'
        'env:print effective DEX_* configuration'
        'compact:concatenate indexable files for LLM prompts'
        'nuke:delete the on-disk index for a project'
        'reindex:drop and re-embed from scratch'
        'mcp:run as an MCP server over stdio'
        'serve:run as an HTTP daemon (multi-project)'
        'watch:keep the index fresh as files change'
        'clone:seed dst index from src'
        'guide:render LLM_GUIDE.md from summaries'
        'hook:Claude Code hook scripts'
        'doctor:check the dex setup'
        'version:print the build version'
        'completion:output shell tab-completion script'
        'setup:guided first-run wizard'
        'config:manage .dex/config.yml'
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
                ask)
                    _arguments \
                        '--intent=[search strategy]:intent:(auto behavior_search symbol_lookup callers callees architecture package_topology editing_context)' \
                        '--k=[max hits per lane]:n:' \
                        '--format=[output format]:fmt:(text json)' \
                        '--no-inline[skip inlining file contents]' \
                        '--max-content-bytes=[truncation limit in bytes]:bytes:' \
                        '-v[verbose: show timing]' \
                        '*:path:_files -/'
                    ;;
                search)
                    local -a sub
                    sub=('semantic:hybrid top-k chunks' 'symbol:exact identifier lookup')
                    _arguments '1: :->sub' '*:: :->rest'
                    case $state in
                        sub)  _describe 'subcommand' sub ;;
                        rest)
                            case $words[1] in
                                semantic)
                                    _arguments \
                                        '--k=[number of results]:n:' \
                                        '--format=[output format]:fmt:(text json)' \
                                        '--rerank=[disable rerank]:r:(off)' \
                                        '--explain[show per-chunk score breakdown]' \
                                        '--max-content-bytes=[truncation limit]:bytes:' \
                                        '-v[verbose]' \
                                        '*:path:_files -/'
                                    ;;
                                symbol)
                                    _arguments \
                                        '--k=[max results]:n:' \
                                        '--format=[output format]:fmt:(text json)' \
                                        '--max-content-bytes=[truncation limit]:bytes:' \
                                        '-v[verbose]' \
                                        '*:path:_files -/'
                                    ;;
                            esac
                            ;;
                    esac
                    ;;
                view)
                    local -a sub
                    sub=('summarize:summarize a file slice via the chat model')
                    _arguments '1: :->sub' '*:: :->rest'
                    case $state in
                        sub) _describe 'subcommand' sub ;;
                        rest)
                            _arguments \
                                '--start=[first line]:line:' \
                                '--end=[last line]:line:' \
                                '--focus=[steering hint]:focus:' \
                                '--format=[output format]:fmt:(text json)' \
                                '-v[verbose]' \
                                '*:path:_files -/'
                            ;;
                    esac
                    ;;
                graph)
                    local -a sub
                    sub=(
                        'neighbors:vector neighbours of a chunk'
                        'deps:imports edges for a file or package'
                        'packages:whole internal package import DAG'
                        'callers:incoming calls edges'
                        'callees:outgoing calls edges'
                        'links:markdown docs this doc links to'
                        'backlinks:markdown docs that link to this doc'
                        'tags:tag→docs or doc→tags'
                        'export:dump nodes/edges as JSONL'
                    )
                    _arguments '1: :->sub' '*:: :->rest'
                    case $state in
                        sub) _describe 'subcommand' sub ;;
                        rest)
                            _arguments \
                                '--k=[max results]:n:' \
                                '--format=[output format]:fmt:(text json)' \
                                '-v[verbose]' \
                                '*:path:_files -/'
                            ;;
                    esac
                    ;;
                index)
                    local -a sub
                    sub=('status:endpoint health and project stats' 'summarize:drain pending summaries')
                    _arguments '1: :->sub' '*:: :->rest'
                    case $state in
                        sub) _describe 'subcommand' sub ;;
                        rest)
                            _arguments \
                                '--graph=[graph phase]:mode:(on off only)' \
                                '--format=[output format]:fmt:(text json)' \
                                '--dry-run[preview what would be indexed without writing]' \
                                '-v[verbose]' \
                                '--force[bypass guards]' \
                                '--summarize[generate summaries]' \
                                '--wait[wait for lock]' \
                                '*:path:_files -/'
                            ;;
                    esac
                    ;;
                hook)
                    local -a sub
                    sub=(
                        'inject:UserPromptSubmit hook — inject dex context'
                        'rewrite:Bash hook — rewrite rg/grep to dex search'
                        'redirect:Read/Grep hook — compress large files'
                        'observe:PostToolUse/Stop hook — append event log'
                    )
                    _arguments '1: :->sub'
                    [[ $state == sub ]] && _describe 'subcommand' sub
                    ;;
                completion)
                    local -a shells
                    shells=('bash' 'zsh' 'fish')
                    _arguments '1: :->shell'
                    [[ $state == shell ]] && _describe 'shell' shells
                    ;;
                env)
                    _arguments \
                        '--all[include tuning knobs]' \
                        '--doc[include documentation strings]' \
                        '-v[verbose (equivalent to --doc)]' \
                        '--format=[output format]:fmt:(text json)'
                    ;;
                doctor)
                    _arguments '-v[verbose]'
                    ;;
                setup)
                    _arguments '--check[non-interactive: exit 0 if setup complete, 1 otherwise]'
                    ;;
                config)
                    local -a sub
                    sub=('init:scaffold .dex/config.yml with commented defaults')
                    _arguments '1: :->sub' '*:: :->rest'
                    case $state in
                        sub) _describe 'subcommand' sub ;;
                        rest)
                            _arguments \
                                '--force[overwrite existing file]' \
                                '--full[include all tuning knobs]'
                            ;;
                    esac
                    ;;
                nuke)
                    _arguments '--yes[skip confirmation]' '*:path:_files -/'
                    ;;
                watch)
                    _arguments \
                        '-v[verbose]' \
                        '--debounce=[quiet window]:duration:' \
                        '--force[bypass guards]' \
                        '*:path:_files -/'
                    ;;
                *)
                    _files -/
                    ;;
            esac
            ;;
    esac
}

_dex "$@"
`

const fishCompletionScript = `# dex fish completions
# dex completion fish > ~/.config/fish/completions/dex.fish

set -l top_cmds ask search view graph index generate env compact nuke reindex mcp serve watch clone guide hook compress-stdin shell-hook doctor version completion setup config

# Top-level commands
complete -c dex -f -n 'not __fish_seen_subcommand_from $top_cmds' -a "$top_cmds"

# --- search subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from search; and not __fish_seen_subcommand_from semantic symbol' -a 'semantic' -d 'hybrid top-k chunks'
complete -c dex -f -n '__fish_seen_subcommand_from search; and not __fish_seen_subcommand_from semantic symbol' -a 'symbol' -d 'exact identifier lookup'

# --- view subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from view; and not __fish_seen_subcommand_from summarize' -a 'summarize' -d 'summarize a file slice'

# --- graph subcommands ---
set -l graph_sub neighbors deps packages callers callees links backlinks tags export
complete -c dex -f -n '__fish_seen_subcommand_from graph; and not __fish_seen_subcommand_from $graph_sub' -a "$graph_sub"

# --- index subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from index; and not __fish_seen_subcommand_from status summarize' -a 'status' -d 'endpoint health and project stats'
complete -c dex -f -n '__fish_seen_subcommand_from index; and not __fish_seen_subcommand_from status summarize' -a 'summarize' -d 'drain pending summaries'

# --- hook subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from hook; and not __fish_seen_subcommand_from inject rewrite redirect observe' -a 'inject' -d 'UserPromptSubmit hook'
complete -c dex -f -n '__fish_seen_subcommand_from hook; and not __fish_seen_subcommand_from inject rewrite redirect observe' -a 'rewrite' -d 'Bash hook'
complete -c dex -f -n '__fish_seen_subcommand_from hook; and not __fish_seen_subcommand_from inject rewrite redirect observe' -a 'redirect' -d 'Read/Grep hook'
complete -c dex -f -n '__fish_seen_subcommand_from hook; and not __fish_seen_subcommand_from inject rewrite redirect observe' -a 'observe' -d 'PostToolUse/Stop hook'

# --- completion subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from completion; and not __fish_seen_subcommand_from bash zsh fish' -a 'bash zsh fish'

# --- config subcommands ---
complete -c dex -f -n '__fish_seen_subcommand_from config; and not __fish_seen_subcommand_from init' -a 'init' -d 'scaffold .dex/config.yml'

# --- ask flags ---
complete -c dex -n '__fish_seen_subcommand_from ask' -l intent -r -a 'auto behavior_search symbol_lookup callers callees architecture package_topology editing_context' -d 'search strategy'
complete -c dex -n '__fish_seen_subcommand_from ask' -l format -r -a 'text json' -d 'output format'
complete -c dex -n '__fish_seen_subcommand_from ask' -l k -r -d 'max hits per lane'
complete -c dex -n '__fish_seen_subcommand_from ask' -l no-inline -d 'skip inlining file contents'
complete -c dex -n '__fish_seen_subcommand_from ask' -l max-content-bytes -r -d 'truncation limit in bytes (0=no limit)'
complete -c dex -n '__fish_seen_subcommand_from ask' -s v -d 'verbose: show timing'

# --- search semantic flags ---
complete -c dex -n '__fish_seen_subcommand_from search' -l format -r -a 'text json' -d 'output format'
complete -c dex -n '__fish_seen_subcommand_from search' -l k -r -d 'number of results'
complete -c dex -n '__fish_seen_subcommand_from search' -l rerank -r -a 'off' -d 'disable rerank for this query'
complete -c dex -n '__fish_seen_subcommand_from search' -l explain -d 'show per-chunk score breakdown'
complete -c dex -n '__fish_seen_subcommand_from search' -l max-content-bytes -r -d 'truncation limit in bytes (0=no limit)'
complete -c dex -n '__fish_seen_subcommand_from search' -s v -d 'verbose'

# --- index flags ---
complete -c dex -n '__fish_seen_subcommand_from index' -l graph -r -a 'on off only' -d 'graph phase mode'
complete -c dex -n '__fish_seen_subcommand_from index' -l format -r -a 'text json' -d 'output format'
complete -c dex -n '__fish_seen_subcommand_from index' -l dry-run -d 'preview without writing'
complete -c dex -n '__fish_seen_subcommand_from index' -s v -d 'verbose'
complete -c dex -n '__fish_seen_subcommand_from index' -l force -d 'bypass guards'
complete -c dex -n '__fish_seen_subcommand_from index' -l summarize -d 'generate summaries'
complete -c dex -n '__fish_seen_subcommand_from index' -l wait -d 'wait for lock'

# --- env flags ---
complete -c dex -n '__fish_seen_subcommand_from env' -l all -d 'include tuning knobs'
complete -c dex -n '__fish_seen_subcommand_from env' -l doc -d 'include documentation'
complete -c dex -n '__fish_seen_subcommand_from env' -s v -d 'verbose (same as --doc)'
complete -c dex -n '__fish_seen_subcommand_from env' -l format -r -a 'text json' -d 'output format'

# --- doctor flags ---
complete -c dex -n '__fish_seen_subcommand_from doctor' -s v -d 'verbose'

# --- setup flags ---
complete -c dex -n '__fish_seen_subcommand_from setup' -l check -d 'exit 0 if setup complete, 1 otherwise'

# --- config init flags ---
complete -c dex -n '__fish_seen_subcommand_from config' -l force -d 'overwrite existing file'
complete -c dex -n '__fish_seen_subcommand_from config' -l full -d 'include all tuning knobs'

# --- nuke flags ---
complete -c dex -n '__fish_seen_subcommand_from nuke' -l yes -d 'skip confirmation'

# --- watch flags ---
complete -c dex -n '__fish_seen_subcommand_from watch' -s v -d 'verbose'
complete -c dex -n '__fish_seen_subcommand_from watch' -l debounce -r -d 'quiet window before re-indexing'
`
