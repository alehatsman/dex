package main

// `dex hook` — Claude Code hook handlers.
//
// Four subcommands, each mapping to a Claude Code hook event:
//
//   inject    UserPromptSubmit  Runs dex ask on the prompt; returns
//                               additionalContext with suggested reads so
//                               relevant files surface before Claude acts.
//
//   rewrite   PreToolUse(Bash)  Rewrites shell commands to dex equivalents:
//                               - rg PATTERN [PATH] (no flags) →
//                                 dex search semantic [PATH] "PATTERN"
//                               - grep [-rniI] PATTERN [PATH] (simple form) →
//                                 appends 2>&1 | dex compress-stdin --command grep
//                               Anything complex passes through unchanged.
//
//   redirect  PreToolUse(Read   For large files (>400 lines), compresses the
//             Grep Search …)    content and redirects to a temp file to cut
//                               token burn. Small files pass through unchanged.
//
//   observe   PostToolUse       Appends a compact event record to
//             Stop              $XDG_DATA_HOME/dex/hooks.jsonl for session
//             PreCompact        awareness. Fire-and-forget; no stdout.
//
// All handlers read JSON from stdin (with a 3 s timeout) and must never
// block Claude — any error silently passes through. Writing to stderr is
// avoided in normal operation so hook noise doesn't pollute the chat.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
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
		return hookRedirect()
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

// ─── inject ────────────────────────────────────────────────────────────────

// hookInject handles UserPromptSubmit. It runs a dex ask query on the prompt
// and emits {"additionalContext": "..."} so Claude sees relevant file paths
// before processing the turn. Silent on any error.
func hookInject(ctx context.Context) error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		return nil
	}

	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	// Skip very short prompts (confirmations, "yes", "ok", etc.) — not
	// worth a round-trip to the index for sub-4-word inputs.
	if len(strings.Fields(payload.Prompt)) < 4 {
		return nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	base, err := indexDir()
	if err != nil {
		return nil
	}
	p, err := proj.Resolve(cwd, base)
	if err != nil {
		return nil
	}

	// 10 s budget — the hook runs synchronously before Claude processes the
	// turn, so latency is visible to the user.
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	s, _ := newServerFromEnv(base)
	_, out, err := s.ContextRouter(tctx, mcp.ContextInput{
		Project:  p.Root,
		Question: payload.Prompt,
		K:        6,
		// NoInline=true: inject only paths + reasons, not raw content.
		// The content would bloat every turn; Claude can Read the files.
		NoInline: true,
	})
	if err != nil || out.Status != "ok" {
		return nil
	}

	ac := buildInjectContext(out)
	if ac == "" {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"additionalContext": ac})
}

func buildInjectContext(out mcp.ContextOutput) string {
	if len(out.SuggestedReads) == 0 && len(out.Symbols) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Dex: relevant context\n\n")

	if len(out.SuggestedReads) > 0 {
		b.WriteString("Suggested reads:\n")
		for _, r := range out.SuggestedReads {
			if r.StartLine > 0 && r.EndLine > 0 {
				fmt.Fprintf(&b, "- %s:%d-%d", r.Path, r.StartLine, r.EndLine)
			} else {
				fmt.Fprintf(&b, "- %s", r.Path)
			}
			if r.Reason != "" {
				fmt.Fprintf(&b, " — %s", r.Reason)
			}
			b.WriteByte('\n')
		}
	}

	if len(out.Symbols) > 0 {
		b.WriteString("\nSymbols:\n")
		for _, sym := range out.Symbols {
			fmt.Fprintf(&b, "- %s %s (%s:%d)\n", sym.Kind, sym.QualifiedName, sym.Path, sym.StartLine)
		}
	}

	if out.NextAction != "" {
		b.WriteString("\nNext: ")
		b.WriteString(out.NextAction)
		b.WriteByte('\n')
	}
	return b.String()
}

// ─── rewrite ──────────────────────────────────────────────────────────────

// hookRewrite handles PreToolUse for Bash: rewrites known search commands to
// dex equivalents so Claude gets semantic results rather than raw grep output.
//
// Rules (applied in order; first match wins):
//
//  rg PATTERN [PATH]         — simple form, no flags
//    → dex search semantic [PATH] "PATTERN"
//
//  grep [-rniI]* PATTERN [PATH] — simple recursive grep, no pipes/redirections
//    → appends 2>&1 | dex compress-stdin --command grep
//
// Anything complex (multiple flags, pipes, semicolons, subshells) passes
// through unchanged. The hook must never break a working command.
func hookRewrite() error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		fmt.Print(hookAllow)
		return nil
	}

	var payload struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Print(hookAllow)
		return nil
	}

	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(payload.ToolInput, &input); err != nil || input.Command == "" {
		fmt.Print(hookAllow)
		return nil
	}

	rewritten, ok := rewriteShellCommand(input.Command)
	if !ok {
		fmt.Print(hookAllow)
		return nil
	}

	// Build updatedInput preserving any other fields in tool_input.
	var inputMap map[string]json.RawMessage
	if err := json.Unmarshal(payload.ToolInput, &inputMap); err != nil {
		fmt.Print(hookAllow)
		return nil
	}
	rewrittenJSON, _ := json.Marshal(rewritten)
	inputMap["command"] = rewrittenJSON

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "allow",
			"updatedInput":       inputMap,
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Print(hookAllow)
	}
	return nil
}

// rewriteShellCommand applies dex rewrite rules to cmd.
// Returns (rewritten, true) when a rewrite applies, ("", false) to pass through.
func rewriteShellCommand(cmd string) (string, bool) {
	// Bail out on any compound command — pipes, semicolons, subshells, redirects.
	// We only rewrite simple single-command invocations.
	if strings.ContainsAny(cmd, "|;&`$(){}") {
		return "", false
	}

	tokens := shellTokenize(cmd)
	if len(tokens) == 0 {
		return "", false
	}

	switch tokens[0] {
	case "rg", "ripgrep":
		return rewriteRg(tokens)
	case "grep":
		return rewriteGrep(cmd, tokens)
	}
	return "", false
}

// rewriteRg rewrites simple `rg PATTERN [PATH]` to `dex search semantic`.
// Only fires for the 2- or 3-token form with no flag arguments.
func rewriteRg(tokens []string) (string, bool) {
	switch len(tokens) {
	case 2:
		// rg PATTERN — search cwd
		if tokens[1][0] == '-' {
			return "", false
		}
		return fmt.Sprintf("dex search semantic . %s", shellQuote(tokens[1])), true
	case 3:
		// rg PATTERN PATH — explicit path
		if tokens[1][0] == '-' || tokens[2][0] == '-' {
			return "", false
		}
		return fmt.Sprintf("dex search semantic %s %s", shellQuote(tokens[2]), shellQuote(tokens[1])), true
	}
	return "", false
}

// rewriteGrep rewrites `grep [-rniI]* PATTERN [PATH]` to pipe through
// compress-stdin. Only simple recursive greps — single block of flags,
// one pattern, optional path, no quotes in the original command.
func rewriteGrep(orig string, tokens []string) (string, bool) {
	if len(tokens) < 3 {
		return "", false
	}

	// First arg must be a flags block (starts with -) containing only r/n/i/I/E/l.
	flags := tokens[1]
	if flags[0] != '-' {
		return "", false
	}
	for _, c := range flags[1:] {
		if !strings.ContainsRune("rniIElh", c) {
			return "", false
		}
	}
	// Must include -r (recursive); non-recursive grep output is usually small.
	if !strings.ContainsRune(flags, 'r') {
		return "", false
	}

	return orig + " 2>&1 | dex compress-stdin --command grep", true
}

// shellTokenize splits a shell command into tokens respecting single/double
// quotes and backslash escapes. Does not handle compound operators.
func shellTokenize(input string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle, inDouble := false, false

	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '\\' && !inSingle && i+1 < len(input):
			i++
			cur.WriteByte(input[i])
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// shellQuote wraps s in double quotes if it contains whitespace or special chars.
func shellQuote(s string) string {
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\'' || c == '"' || c == '\\' {
			return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
		}
	}
	return s
}

// ─── redirect ─────────────────────────────────────────────────────────────

// redirectLineThreshold is the line count above which a file is compressed
// and redirected to a temp path. Below this the Read passes through as-is.
const redirectLineThreshold = 400

// hookRedirect handles PreToolUse for Read tools. Large files are run through
// dex's compression pipeline and redirected to a temp file; the original
// file is never modified.
func hookRedirect() error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		fmt.Print(hookAllow)
		return nil
	}

	var payload struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Print(hookAllow)
		return nil
	}

	switch payload.ToolName {
	case "Read", "read", "ReadFile", "read_file":
		redirectFileRead(payload.ToolInput)
	default:
		fmt.Print(hookAllow)
	}
	return nil
}

func redirectFileRead(rawInput json.RawMessage) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil || input.Path == "" {
		fmt.Print(hookAllow)
		return
	}

	content, err := os.ReadFile(input.Path)
	if err != nil {
		fmt.Print(hookAllow)
		return
	}
	if strings.Count(string(content), "\n")+1 < redirectLineThreshold {
		fmt.Print(hookAllow)
		return
	}

	compressed, _, _ := mcp.CompressText(string(content), "", 0)
	// Only redirect when compression saves at least 20% of lines.
	origLines := strings.Count(string(content), "\n") + 1
	compLines := strings.Count(compressed, "\n") + 1
	if compLines*100/origLines > 80 {
		fmt.Print(hookAllow)
		return
	}

	tmp, err := os.CreateTemp("", "dex-redirect-*"+filepath.Ext(input.Path))
	if err != nil {
		fmt.Print(hookAllow)
		return
	}
	if _, err := tmp.WriteString(compressed); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		fmt.Print(hookAllow)
		return
	}
	_ = tmp.Close()

	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "allow",
			"updatedInput":       map[string]string{"path": tmp.Name()},
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Print(hookAllow)
	}
}

// ─── observe ──────────────────────────────────────────────────────────────

// hookObserve handles PostToolUse, Stop, and PreCompact. It appends a compact
// event record to $XDG_DATA_HOME/dex/hooks.jsonl. No stdout output.
func hookObserve() error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		return nil
	}

	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}

	type event struct {
		TS       int64  `json:"ts"`
		ToolName string `json:"tool_name,omitempty"`
		Tokens   int    `json:"tokens,omitempty"`
	}
	ev := event{TS: time.Now().Unix()}

	if raw, ok := v["tool_name"]; ok {
		json.Unmarshal(raw, &ev.ToolName) //nolint:errcheck
	}
	if raw, ok := v["tool_input"]; ok {
		ev.Tokens = len(raw) / 4 // rough 4-bytes-per-token estimate
	}

	logDir := hookLogDir()
	if logDir == "" {
		return nil
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(logDir, "hooks.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
	return nil
}

func hookLogDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "dex")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "dex")
}
