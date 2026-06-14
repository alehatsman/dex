// Completion-script body generators (#482, phase 2 of #469).
//
// #469 generated only the top-level command LIST from the registry; the
// per-command bodies — zsh `_arguments` blocks, bash subcommand/flag-value
// cases, fish per-flag completions — were still hand-maintained and drift-prone.
// These generators render those bodies from the same registry (registry.go), so
// flags, choices, and subcommands live in exactly one place for all three shells.
package main

import (
	"fmt"
	"strings"
)

// ---- zsh ----------------------------------------------------------------

// zshArgBlocks renders the per-command `_arguments` case arms for the zsh
// completion's `args` state. Indentation matches the surrounding template
// (arms at 16 columns, bodies at 20).
func zshArgBlocks() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		fmt.Fprintf(&b, "                %s)\n", v.name)
		b.WriteString(zshVerbBody(v))
		b.WriteString("                    ;;\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// zshVerbBody renders the body of one verb's case arm: a sub-dispatch block
// when the verb has subcommands, otherwise a flat `_arguments` over its flags
// (and a trailing path argument unless the verb takes no files).
func zshVerbBody(v verbSpec) string {
	if len(v.subs) > 0 {
		return zshSubBody(v)
	}
	specs := zshArgSpecs(v)
	if len(specs) == 0 {
		return "" // argless verb (e.g. mcp, version) — nothing to complete
	}
	return renderZshArguments(specs, 20)
}

// zshArgSpecs returns the `_arguments` optspecs for a verb: one per flag, plus
// a trailing path spec unless noFiles.
func zshArgSpecs(v verbSpec) []string {
	specs := make([]string, 0, len(v.flags)+1)
	for _, f := range v.flags {
		specs = append(specs, zshFlagSpec(f))
	}
	specs = append(specs, zshPathSpec(v)...)
	return specs
}

// zshPathSpec returns the trailing path optspec (or nothing for noFiles verbs).
func zshPathSpec(v verbSpec) []string {
	switch {
	case v.noFiles:
		return nil
	case v.fileArgs:
		return []string{"'*:path:_files'"}
	default:
		return []string{"'*:path:_files -/'"}
	}
}

// zshFlagSpec renders one flag as a zsh `_arguments` optspec.
func zshFlagSpec(f flagSpec) string {
	desc := zshEscape(f.desc)
	if !f.arg {
		return fmt.Sprintf("'%s[%s]'", f.name, desc)
	}
	tag := strings.TrimLeft(f.name, "-")
	if len(f.choices) > 0 {
		return fmt.Sprintf("'%s=[%s]:%s:(%s)'", f.name, desc, tag, strings.Join(f.choices, " "))
	}
	return fmt.Sprintf("'%s=[%s]:%s:'", f.name, desc, tag)
}

// zshSubBody renders the sub-dispatch block for a verb with subcommands.
func zshSubBody(v verbSpec) string {
	const pad = "                    "      // 20
	const ipad = "                        " // 24
	var b strings.Builder
	b.WriteString(pad + "local -a sub\n")
	b.WriteString(pad + "sub=(\n")
	for _, s := range v.subs {
		if s.desc == "" {
			fmt.Fprintf(&b, "%s'%s'\n", ipad, s.name)
		} else {
			fmt.Fprintf(&b, "%s'%s:%s'\n", ipad, s.name, zshEscape(s.desc))
		}
	}
	b.WriteString(pad + ")\n")

	rest := make([]string, 0, len(v.flags)+1)
	for _, f := range v.flags {
		rest = append(rest, zshFlagSpec(f))
	}
	rest = append(rest, zshPathSpec(v)...)

	if len(rest) == 0 {
		b.WriteString(pad + "_arguments '1: :->sub'\n")
		b.WriteString(pad + "[[ $state == sub ]] && _describe 'subcommand' sub\n")
		return b.String()
	}
	b.WriteString(pad + "_arguments '1: :->sub' '*:: :->rest'\n")
	b.WriteString(pad + "case $state in\n")
	b.WriteString(ipad + "sub) _describe 'subcommand' sub ;;\n")
	b.WriteString(ipad + "rest)\n")
	b.WriteString(renderZshArguments(rest, 28))
	b.WriteString("                            ;;\n") // 28
	b.WriteString(pad + "esac\n")
	return b.String()
}

// renderZshArguments emits an `_arguments \` invocation with one optspec per
// line, backslash-continued, indented to the given column.
func renderZshArguments(specs []string, indent int) string {
	pad := strings.Repeat(" ", indent)
	spad := strings.Repeat(" ", indent+4)
	var b strings.Builder
	b.WriteString(pad + "_arguments \\\n")
	for i, s := range specs {
		b.WriteString(spad + s)
		if i < len(specs)-1 {
			b.WriteString(" \\")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// zshEscape neutralises the characters that would break a single-quoted zsh
// optspec: the quote itself and the `:` field separator.
func zshEscape(s string) string {
	s = strings.ReplaceAll(s, "'", `'\''`)
	s = strings.ReplaceAll(s, ":", `\:`)
	return s
}

// ---- bash ---------------------------------------------------------------

// bashSubcmdCases renders the depth-2 subcommand `case $cmd in` arms.
func bashSubcmdCases() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden || len(v.subs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "            %s) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")); return ;;\n",
			v.name, strings.Join(subNames(v), " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// bashFlagNameCases renders the `case $cmd in` arms that complete flag NAMES
// (triggered when the current word starts with a dash).
func bashFlagNameCases() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden || len(v.flags) == 0 {
			continue
		}
		fmt.Fprintf(&b, "            %s) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")); return ;;\n",
			v.name, strings.Join(flagNames(v), " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// bashFlagValueCases renders the command-scoped flag-VALUE completion: for each
// verb with choice-bearing flags, both the space form (`--flag val`) and the
// equals form (`--flag=val`). Scoping by command avoids the --mode collision
// (read's modes vs compress's modes).
func bashFlagValueCases() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		choiceFlags := flagsWithChoices(v)
		if len(choiceFlags) == 0 {
			continue
		}
		fmt.Fprintf(&b, "            %s)\n", v.name)
		b.WriteString("                case $prev in\n")
		for _, f := range choiceFlags {
			fmt.Fprintf(&b, "                    %s) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")); return ;;\n",
				f.name, strings.Join(f.choices, " "))
		}
		b.WriteString("                esac\n")
		b.WriteString("                case $cur in\n")
		for _, f := range choiceFlags {
			fmt.Fprintf(&b, "                    %s=*) COMPREPLY=($(compgen -W \"%s\" -- \"${cur#*=}\")); COMPREPLY=(\"${COMPREPLY[@]/#/%s=}\"); return ;;\n",
				f.name, strings.Join(f.choices, " "), f.name)
		}
		b.WriteString("                esac\n")
		b.WriteString("                ;;\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- fish ---------------------------------------------------------------

// fishTopCommands renders one `complete` per advertised verb so each top-level
// command carries its summary as a description.
func fishTopCommands() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden {
			continue
		}
		fmt.Fprintf(&b, "complete -c dex -f -n 'not __fish_seen_subcommand_from $top_cmds' -a '%s' -d '%s'\n",
			v.name, fishEscape(v.summary))
	}
	return strings.TrimRight(b.String(), "\n")
}

// fishSubcmdCompletions renders the per-subcommand completions, guarded so they
// only fire before a subcommand is chosen.
func fishSubcmdCompletions() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden || len(v.subs) == 0 {
			continue
		}
		all := strings.Join(subNames(v), " ")
		fmt.Fprintf(&b, "# --- %s subcommands ---\n", v.name)
		for _, s := range v.subs {
			line := fmt.Sprintf("complete -c dex -f -n '__fish_seen_subcommand_from %s; and not __fish_seen_subcommand_from %s' -a '%s'",
				v.name, all, s.name)
			if s.desc != "" {
				line += fmt.Sprintf(" -d '%s'", fishEscape(s.desc))
			}
			b.WriteString(line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// fishFlagCompletions renders the per-flag completions, scoped to the verb.
func fishFlagCompletions() string {
	var b strings.Builder
	for _, v := range verbs {
		if v.group == groupHidden || len(v.flags) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# --- %s flags ---\n", v.name)
		for _, f := range v.flags {
			parts := []string{
				"complete -c dex",
				fmt.Sprintf("-n '__fish_seen_subcommand_from %s'", v.name),
			}
			if strings.HasPrefix(f.name, "--") {
				parts = append(parts, "-l "+strings.TrimPrefix(f.name, "--"))
			} else {
				parts = append(parts, "-s "+strings.TrimPrefix(f.name, "-"))
			}
			if f.arg {
				parts = append(parts, "-r")
			}
			if len(f.choices) > 0 {
				parts = append(parts, fmt.Sprintf("-a '%s'", strings.Join(f.choices, " ")))
			}
			if f.desc != "" {
				parts = append(parts, fmt.Sprintf("-d '%s'", fishEscape(f.desc)))
			}
			b.WriteString(strings.Join(parts, " ") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// fishEscape neutralises the single quote inside a fish single-quoted string.
func fishEscape(s string) string {
	return strings.ReplaceAll(s, "'", `\'`)
}

// ---- shared helpers -----------------------------------------------------

func subNames(v verbSpec) []string {
	out := make([]string, len(v.subs))
	for i, s := range v.subs {
		out[i] = s.name
	}
	return out
}

func flagNames(v verbSpec) []string {
	out := make([]string, len(v.flags))
	for i, f := range v.flags {
		out[i] = f.name
	}
	return out
}

func flagsWithChoices(v verbSpec) []flagSpec {
	var out []flagSpec
	for _, f := range v.flags {
		if len(f.choices) > 0 {
			out = append(out, f)
		}
	}
	return out
}
