package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func splitProjectArg(args []string) (path string, rest []string) {
	if len(args) > 0 {
		if st, err := os.Stat(args[0]); err == nil && st.IsDir() {
			return args[0], args[1:]
		}
	}
	return ".", args
}

// validIntent reports whether s is one of the strategies the context
// router accepts. Empty string means "auto" and is allowed.
func validIntent(s string) bool {
	switch s {
	case "", "auto", "behavior_search", "symbol_lookup", "callers", "callees",
		"architecture", "package_topology", "editing_context", "assemble":
		return true
	}
	return false
}

// validExpandMode accepts the query-side expansion levels (#252). The empty
// string is valid and defers to the server default (DEX_EXPAND_MODE), matching
// the MCP `ask` tool's expand field. Normalisation mirrors
// retrieve.ResolveExpandMode (trim + case-fold) so `--expand=FULL` and
// `--expand=" on "` resolve the same on the CLI as over MCP.
func validExpandMode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "on", "full":
		return true
	}
	return false
}

// boolFlag duck-types the stdlib's unexported flag.boolFlag interface so
// reorderFlags can tell standalone boolean flags (`-v`) from flags that
// consume a value as the next token (`--rerank off`).
type boolFlag interface {
	flag.Value
	IsBoolFlag() bool
}

// reorderFlags moves every flag-shaped token to the front of args so
// flag.Parse sees them even when the user typed them after positional
// args. Without this, Go's flag package silently stops parsing at the
// first non-flag arg and quietly drops every flag that follows — a
// real footgun for invocations like `dex search <path> "q" --k=3`.
//
// Uses the FlagSet to detect which flags consume a separate-token value
// (so `--rerank off` is treated as one flag/value pair, not flag plus
// stray positional). `--` ends flag scanning, matching stdlib behavior.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag — let fs.Parse raise the error.
			continue
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

// setHelp wires `<cmd> -h` to print a one-line summary, a usage pattern
// showing positional args, the auto-generated flag defaults, optional
// examples, and a pointer to the full reference.
// The variadic examples parameter accepts concrete invocation lines; each
// is printed under an "examples:" header. Pass none to omit that section.
func setHelp(fs *flag.FlagSet, oneLiner, usagePattern string, examples ...string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, oneLiner)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "usage:")
		fmt.Fprintln(out, "  "+usagePattern)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "flags:")
			fs.PrintDefaults()
		}
		if len(examples) > 0 {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "examples:")
			for _, ex := range examples {
				fmt.Fprintln(out, "  "+ex)
			}
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "see also: dex help all")
	}
}

// ─── env helpers ──────────────────────────────────────────────────────────
