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
//   redirect  PreToolUse(Read   For large indexed code files (>400 lines),
//             Grep Search …)    renders a signatures view (imports + top-level
//                               declarations, bodies dropped) from the graph
//                               index and redirects to a temp file. Files that
//                               are small, unindexed, or have no graph symbols
//                               pass through unchanged.
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

// ─── inject ────────────────────────────────────────────────────────────────

// hookInject handles UserPromptSubmit. It runs a dex ask query on the prompt
// and emits {"additionalContext": "..."} so Claude sees relevant file paths
// before processing the turn. Also prepends a one-time-per-session nudge when
// routing rules are stale or drifted. Silent on any error.
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

	nudge := rulesNudge()

	// Skip very short prompts (confirmations, "yes", "ok", etc.) — not
	// worth a round-trip to the index for sub-4-word inputs.
	if len(strings.Fields(payload.Prompt)) < 4 {
		return emitInjectContext(nudge, "")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return emitInjectContext(nudge, "")
	}
	base, err := indexDir()
	if err != nil {
		return emitInjectContext(nudge, "")
	}
	p, err := proj.Resolve(cwd, base)
	if err != nil {
		return emitInjectContext(nudge, "")
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
		return emitInjectContext(nudge, "")
	}

	return emitInjectContext(nudge, buildInjectContext(out))
}

// emitInjectContext encodes additionalContext combining nudge and ac.
// Emits nothing and returns nil when both are empty.
func emitInjectContext(nudge, ac string) error {
	combined := nudge
	if ac != "" {
		if combined != "" {
			combined += "\n\n"
		}
		combined += ac
	}
	if combined == "" {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"additionalContext": combined})
}

// rulesNudge returns a one-time-per-session warning when routing rules are
// stale or drifted. Returns "" when rules are in sync or the nudge already
// fired recently (debounced by a sentinel file with an 8 h TTL). Fails open.
func rulesNudge() string {
	st, _ := checkRulesStatus()
	if st == rulesInSync {
		return ""
	}

	sentinel := rulesNudgeSentinelPath()
	if sentinel != "" {
		if fi, err := os.Stat(sentinel); err == nil {
			if time.Since(fi.ModTime()) < 8*time.Hour {
				return "" // already nudged this session
			}
		}
		// Touch sentinel before emitting so concurrent hook invocations don't double-fire.
		if mkErr := os.MkdirAll(filepath.Dir(sentinel), 0o755); mkErr == nil {
			if f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
				_ = f.Close()
			}
		}
	}

	switch st {
	case rulesMissing, rulesNoMarkers:
		return "[DEX] routing rules not installed — run `dex setup`"
	case rulesStale:
		return "[DEX] routing rules are outdated — run `dex setup`"
	case rulesDrifted:
		return "[DEX] routing rules have drifted from canonical — run `dex setup` to restore"
	}
	return ""
}

func rulesNudgeSentinelPath() string {
	dir := hookLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "rules-nudge-sentinel")
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
//	rg PATTERN [PATH]         — simple form, no flags
//	  → dex search semantic [PATH] "PATTERN"
//
//	grep [-rniI]* PATTERN [PATH] — simple recursive grep, no pipes/redirections
//	  → appends 2>&1 | dex compress-stdin --command grep
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
	rewrittenJSON, err := json.Marshal(rewritten)
	if err != nil {
		fmt.Print(hookAllow)
		return nil
	}
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

// redirectLineThreshold is the line count above which a code file is
// redirected to a signatures view. Below this the Read passes through as-is —
// small files are cheap to read in full.
const redirectLineThreshold = 400

// hookRedirect handles PreToolUse for Read tools. Large indexed code files are
// rendered to a signatures view (imports + declarations, bodies dropped) and
// redirected to a temp file; the original file is never modified. Anything
// that can't be turned into a useful signatures view passes through.
func hookRedirect(ctx context.Context) error {
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
		redirectFileRead(ctx, payload.ToolInput)
	default:
		fmt.Print(hookAllow)
	}
	return nil
}

func redirectFileRead(ctx context.Context, rawInput json.RawMessage) {
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
	lines := strings.Split(string(content), "\n")
	if len(lines) < redirectLineThreshold {
		fmt.Print(hookAllow)
		return
	}

	view := buildSignaturesView(ctx, input.Path, lines)
	if view == "" {
		// Not indexed, no symbols, or non-code file — read it normally.
		fmt.Print(hookAllow)
		return
	}

	tmp, err := os.CreateTemp("", "dex-redirect-*"+filepath.Ext(input.Path))
	if err != nil {
		fmt.Print(hookAllow)
		return
	}
	if _, err := tmp.WriteString(view); err != nil {
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

// buildSignaturesView renders a signatures view for an indexed code file:
// a header, then one declaration line per top-level symbol (function, method,
// type, struct, interface) in source order, with bodies dropped. Returns ""
// when the project isn't indexed, the file has no graph symbols, or anything
// fails — the caller falls back to a normal Read.
func buildSignaturesView(ctx context.Context, absPath string, lines []string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	base, err := indexDir()
	if err != nil {
		return ""
	}
	// The index is built for the project root, which is the Claude session's
	// cwd — resolve from there, not from the file's directory (proj.Resolve
	// treats the path it's given AS the root; it does not walk up to .git).
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	p, err := proj.Resolve(cwd, base)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		return ""
	}
	abs, err := filepath.Abs(absPath)
	if err != nil {
		return ""
	}
	relPath, err := filepath.Rel(p.Root, abs)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return ""
	}
	relPath = filepath.ToSlash(relPath)

	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return ""
	}
	defer func() { _ = st.Close() }()

	syms, err := st.SymbolsByFile(ctx, relPath)
	if err != nil || len(syms) == 0 {
		return ""
	}

	var b strings.Builder
	var rendered int
	var body strings.Builder
	for _, sym := range syms {
		// Only top-level declarations — skip struct fields, imports, headings,
		// and other body/structure detail that would bloat the view.
		if !signatureKind(sym.Kind) {
			continue
		}
		// start_line is 1-based; declaration sits at lines[start-1].
		if sym.StartLine < 1 || sym.StartLine > len(lines) {
			continue
		}
		decl := strings.TrimRight(lines[sym.StartLine-1], " \t")
		if decl == "" {
			continue
		}
		fmt.Fprintf(&body, "%s\t// :%d %s\n", decl, sym.StartLine, sym.Kind)
		rendered++
	}
	if rendered == 0 {
		return ""
	}

	fmt.Fprintf(&b, "// dex signatures view — %s (%d lines, %d declarations)\n", relPath, len(lines), rendered)
	b.WriteString("// Bodies dropped. Read with a narrower line range, or use `dex view summarize`, for full detail.\n\n")
	b.WriteString(body.String())
	return b.String()
}

// signatureKind reports whether a graph node kind is a top-level declaration
// worth emitting in the signatures view (vs. fields, imports, headings, etc.).
func signatureKind(kind string) bool {
	switch kind {
	case "function", "method", "struct", "interface", "type", "class":
		return true
	}
	return false
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
		_ = json.Unmarshal(raw, &ev.ToolName)
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
