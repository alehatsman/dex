package main

// `dex hook` — Claude Code hook handlers.
//
// Three subcommands, each mapping to a Claude Code hook event:
//
//   inject    UserPromptSubmit  Runs dex ask on the prompt; returns
//                               additionalContext with suggested reads so
//                               relevant files surface before Claude acts.
//
//   redirect  PreToolUse(Read)  For large files (>400 lines), compresses the
//                               content and redirects to a temp file to cut
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
		return fmt.Errorf("hook needs a subcommand: inject | redirect | observe")
	}
	switch args[0] {
	case "inject":
		return hookInject(ctx)
	case "redirect":
		return hookRedirect()
	case "observe":
		return hookObserve()
	default:
		return fmt.Errorf("unknown hook subcommand %q (want inject|redirect|observe)", args[0])
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
