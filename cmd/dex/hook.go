package main

// `dex hook` — Claude Code hook handlers.
//
// Four subcommands, each mapping to a Claude Code hook event:
//
//	inject    UserPromptSubmit  Runs dex ask on the prompt; returns
//	                            additionalContext with suggested reads so
//	                            relevant files surface before Claude acts.
//
//	rewrite   PreToolUse(Bash)  Rewrites shell commands to dex equivalents:
//	                            - rg PATTERN [PATH] (no flags) →
//	                              dex find [PATH] "PATTERN"
//	                            - grep [-rniI] PATTERN [PATH] (simple form) →
//	                              appends 2>&1 | dex compress-stdin --command grep
//	                            Anything complex passes through unchanged.
//
//	redirect  PreToolUse(Read   For large indexed code files (>400 lines),
//	          Grep Search …)    renders a signatures view (imports + top-level
//	                            declarations, bodies dropped) from the graph
//	                            index and redirects to a temp file. Files that
//	                            are small, unindexed, or have no graph symbols
//	                            pass through unchanged.
//
//	observe   PostToolUse       Appends a compact event record to
//	          Stop              $XDG_DATA_HOME/dex/hooks.jsonl for session
//	          PreCompact        awareness. Fire-and-forget; no stdout.
//
// All handlers read JSON from stdin (with a 3 s timeout) and must never
// block Claude — any error silently passes through. Writing to stderr is
// avoided in normal operation so hook noise doesn't pollute the chat.
//
// Implementations are split across:
//   - hook_inject.go   (UserPromptSubmit — context injection)
//   - hook_rewrite.go  (PreToolUse Bash — command rewriting)
//   - hook_redirect.go (PreToolUse Read — signatures view redirect)
//   - hook_observe.go  (PostToolUse/Stop/PreCompact — event logging)

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// cmdHook dispatches `dex hook <subcommand>`.
func cmdHook(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("hook needs a subcommand: inject | rewrite | redirect | observe")
	}
	switch args[0] {
	case "inject":
		return hookInject(ctx)
	case "rewrite":
		return hookRewrite()
	case "redirect":
		return hookRedirect(ctx)
	case "observe":
		return hookObserve()
	default:
		return fmt.Errorf("unknown hook subcommand %q (want inject|rewrite|redirect|observe)", args[0])
	}
}

// hookAllow is the PreToolUse pass-through response.
const hookAllow = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`

// hookReadStdin drains stdin with a 3 s timeout.
func hookReadStdin() []byte {
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(os.Stdin)
		ch <- b
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		return nil
	}
}
