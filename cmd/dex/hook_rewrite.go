package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// hookRewrite handles PreToolUse for Bash: rewrites known search commands to
// dex equivalents so Claude gets semantic results rather than raw grep output.
//
// Rules (applied in order; first match wins):
//
//	rg PATTERN [PATH]         — simple form, no flags
//	  → dex search [PATH] "PATTERN"
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

// rewriteRg rewrites simple `rg PATTERN [PATH]` to `dex search`.
// Only fires for the 2- or 3-token form with no flag arguments.
func rewriteRg(tokens []string) (string, bool) {
	switch len(tokens) {
	case 2:
		// rg PATTERN — search cwd
		if tokens[1][0] == '-' {
			return "", false
		}
		return fmt.Sprintf("dex search . %s", shellQuote(tokens[1])), true
	case 3:
		// rg PATTERN PATH — explicit path
		if tokens[1][0] == '-' || tokens[2][0] == '-' {
			return "", false
		}
		return fmt.Sprintf("dex search %s %s", shellQuote(tokens[2]), shellQuote(tokens[1])), true
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
