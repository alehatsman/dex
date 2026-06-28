// dex — local semantic-search helper for Claude Code.
//
// The query-side verbs share the MCP tool names 1:1 (search, trace, repo_map,
// read, ask). The build/maintenance commands are CLI-only.
//
//	ask <path> <q...>             Primary entry point (MCP: ask).
//	search <path> <q...>          Hybrid semantic top-k chunks; fuses exact
//	                              symbol-name hits via RRF (MCP: search).
//	trace <path> <sym>            Call-graph callers/callees/path/impact (MCP: trace).
//	graph neighbors <path> <file> <line>
//	                              Vector neighbours of a chunk (CLI-only).
//	graph deps <path> [--file|--package]
//	                              `imports` edges for a file/package (MCP: deps).
//	graph links <path> <doc>      Docs this markdown doc links to (CLI-only).
//	graph backlinks <path> <doc>  Docs that link to this markdown doc (CLI-only).
//	graph tags <path> --tag|--doc Tag→docs or doc→tags (CLI-only).
//	graph export <path>           Dump nodes/edges as JSONL (CLI-only).
//	read <file>                   Read a file (MCP: read). Default mode=full is raw (no LLM); mode=summary is an LLM digest.
//	grep <path> <pattern>         Exact RE2 regex search over project files (MCP: grep).
//	shell <command...>            Run a command with compressed output (MCP: shell).
//	index <path>                  Build or refresh the per-project index.
//	index status [<path>]         Endpoint health + indexed projects (MCP: status).
//	env                           Print effective env-var configuration.
//	compact <path>                Concatenate indexable files for LLM prompts (alias: bundle).
//	nuke <path>                   Delete the on-disk index for a project.
//	reindex <path>|--all          Drop and re-embed.
//	watch <path>                  Keep the index fresh as files change.
//	clone <src> <dst>             Seed dst's index from src's (worktrees).
//	hook inject                   Claude Code UserPromptSubmit hook — injects dex context.
//	hook rewrite                  Claude Code PreToolUse(Bash) hook — rewrites rg/grep to dex.
//	hook redirect                 Claude Code PreToolUse(Read/Grep/…) hook — compresses large files.
//	hook observe                  Claude Code PostToolUse/Stop hook — appends event log.
//	bench locomo <path>           LoCoMo memory-recall benchmark (recall@k / token-F1).
//	notes add|query|rm|gc         Store/list/delete/gc per-project facts (MCP: notes).
//	compress <file|->             Compress a file or stdin through the dex engine (no LLM).
//	compress-stdin                Compress stdin through dex patterns; writes to stdout.
//	shell-hook                    Print eval-able shell hook for passive output compression.
//	setup                         Guided first-run wizard: check endpoints, index cwd, write Claude routing rules.
//	mcp                           Run as an MCP server over stdio. Tool surface is capability-derived.
//	version                       Print the build version.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		// TTY: friendly quickstart. Non-TTY: short error.
		if stdinIsTTY() {
			usageQuickstart()
		} else {
			fmt.Fprintln(os.Stderr, "dex: no command given — run 'dex help'")
		}
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	// Fill DEX_* gaps from .dex/config.yml in the working dir (env still wins).
	// Collapses the env-var sprawl into one per-project file; see config_file.go.
	if wd, werr := os.Getwd(); werr == nil {
		if cerr := applyProjectConfig(wd); cerr != nil {
			fmt.Fprintf(os.Stderr, "dex: %v\n", cerr)
			os.Exit(2)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := dispatchCmd(ctx, cmd, args)
	if err != nil {
		// `-h` returns flag.ErrHelp via flag.ContinueOnError. The FlagSet
		// already printed its usage block; suppress the redundant
		// "flag: help requested" line and exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// SIGINT/SIGTERM cancel ctx; report a friendlier exit (130 is
		// the conventional shell code for SIGINT).
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
