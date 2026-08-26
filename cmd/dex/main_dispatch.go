package main

import (
	"context"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/mcp"
)

// dispatchCmd routes a top-level command to its handler.
// Version and help print and return nil. Unknown commands print usage and exit.
func dispatchCmd(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "version", "-V", "--version", "-v":
		// -v/-V/--version as the top-level command map to `version` (#505).
		// A bare `dex -v` is unambiguous here; per-subcommand `-v` is parsed
		// by each subcommand's own flagset and never reaches here.
		fmt.Println(mcp.Version)
		return nil
	case "-h", "--help":
		usageConcise()
		return nil
	case "help":
		if len(args) > 0 && args[0] == "all" {
			usageFull()
		} else {
			usageConcise()
		}
		return nil
	}

	// noCtx wraps a no-context handler to match the dispatch signature.
	noCtx := func(fn func([]string) error) func(context.Context, []string) error {
		return func(_ context.Context, a []string) error { return fn(a) }
	}

	dispatch := map[string]func(context.Context, []string) error{
		"index":          cmdIndexDispatch,
		"status":         cmdIndexStatus, // top-level alias for `dex index status` (#501)
		"ask":            cmdAsk,
		"summarize":      cmdSummarize,
		"read":           cmdRead,
		"graph":          cmdGraph,
		"repo_map":       cmdMap,
		"search":         cmdFind,
		"trace":          cmdTrace,
		"review_diff":    cmdReview,
		"locate":         cmdLocate,
		"plan_rename":    cmdRefactor,
		"rehearse_patch": cmdRehearse,
		"cohort":         cmdCohort,
		"refs":           cmdRefs,
		"check":          cmdCheck,
		"grep":           cmdGrep,
		"env":            cmdEnv,
		"compact":        cmdCompact,
		"nuke":           cmdNuke,
		"reindex":        cmdReindex,
		"mcp":            cmdMCP,
		"serve":          cmdServe,
		"watch":          cmdWatch,
		"clone":          cmdClone,
		"hook":           cmdHook,
		"compress":       noCtx(cmdCompress),
		"compress-stdin": noCtx(cmdCompressStdin),
		"shell-hook":     noCtx(cmdShellHook),
		"setup":          cmdSetup,
		"doctor":         cmdDoctor,
		"completion":     noCtx(cmdCompletion),
		"bench":          runBench,
		"feedback":       runFeedback,
		"config":         noCtx(cmdConfig),
	}

	if fn, ok := dispatch[cmd]; ok {
		return fn(ctx, args)
	}
	if hint, ok := mcpOnlyToolHint(cmd); ok {
		fmt.Fprintln(os.Stderr, hint)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
	usageConcise()
	os.Exit(2)
	return nil // unreachable
}
