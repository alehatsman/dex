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

// registerProjectFlag adds the --project flag shared by the `notes` verbs so the
// project root can be given as `--project <dir>` in addition to the positional
// `<path>` (#89). dex already uses --project (serve) and --project-root (mcp)
// elsewhere; honouring it here removes a papercut where `dex notes add --project
// <dir>` failed with a cryptic "flag provided but not defined". Resolve the
// effective root with projectFromFlag after Parse.
func registerProjectFlag(fs *flag.FlagSet) *string {
	return fs.String("project", "", "project root (default: cwd; also accepted as a positional <path>)")
}

// projectFromFlag resolves the effective project path for a notes verb: the
// --project flag wins when set, else the positional <path> parsed from the
// remaining args. Both paths flow through the same store resolution downstream.
func projectFromFlag(flagVal string, args []string) (path string, rest []string) {
	path, rest = splitProjectArg(args)
	if v := strings.TrimSpace(flagVal); v != "" {
		path = v
	}
	return path, rest
}

// parseNotesArgs registers the shared --project flag on fs, parses args
// (reordering flags after positionals), and resolves the effective project path
// (--project wins over the positional <path>). The notes verbs whose project is
// the only positional funnel through it so `--project <dir>` works uniformly
// (#89). Verbs with extra post-parse validation (e.g. add) call
// registerProjectFlag + projectFromFlag directly instead.
func parseNotesArgs(fs *flag.FlagSet, args []string) (path string, rest []string, err error) {
	projectRoot := registerProjectFlag(fs)
	if err = fs.Parse(reorderFlags(fs, args)); err != nil {
		return "", nil, err
	}
	path, rest = projectFromFlag(*projectRoot, fs.Args())
	return path, rest, nil
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

// canonicalArchetype validates a knowledge-fact archetype case-insensitively
// and returns its canonical capitalisation. The canonical form matters: the
// store's salience weighting (archetypeWeight) is case-sensitive, so a
// mis-cased archetype would silently fall back to the default weight (#520).
func canonicalArchetype(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "architecture":
		return "Architecture", true
	case "gotcha":
		return "Gotcha", true
	case "decision":
		return "Decision", true
	case "convention":
		return "Convention", true
	case "dependency":
		return "Dependency", true
	case "pattern":
		return "Pattern", true
	case "fact":
		return "Fact", true
	case "reviewfinding", "review-finding", "review_finding":
		return "ReviewFinding", true
	case "observation":
		return "Observation", true
	case "hypothesis":
		return "Hypothesis", true
	case "inference":
		return "Inference", true
	case "verifiedfact", "verified-fact", "verified_fact":
		return "VerifiedFact", true
	}
	return "", false
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
