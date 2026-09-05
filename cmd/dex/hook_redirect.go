package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/proj"
)

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
	b.WriteString("// Bodies dropped. Read with a narrower line range, or `dex query --kind=read --want=full` for the full file.\n\n")
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
