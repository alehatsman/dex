package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/tokens"
)

// cmdCompress is the first-class `dex compress <file|->` verb. It runs the
// internal/compress engine on a file or stdin and writes the compressed text
// to stdout (or --out), with no LLM call. This is the documented, flagged
// entrypoint to the same engine the redirect hook uses internally; the older
// `compress-stdin` command stays as the hook-internal log-output path.
//
// Modes operate on raw text and need no index: aggressive (strip comments +
// structural noise + per-language entropy + symbol map), entropy (drop
// low-information lines), terse (function-word/abbreviation/dedup passes), and
// auto (aggressive, which self-degrades to the original via SafeguardRatio when
// it would not help). Index-backed structural views (signatures/map) live in
// `dex read`.
func cmdCompress(args []string) error {
	fs := flag.NewFlagSet("compress", flag.ContinueOnError)
	setHelp(fs,
		"Compress a file or stdin through the dex engine (no LLM call).",
		"dex compress [flags] <file|->",
		`dex compress main.go`,
		`dex compress --mode=entropy build.log`,
		`go test ./... 2>&1 | dex compress - --mode=terse`,
		`dex compress --format=json main.go`,
	)
	mode := fs.String("mode", "auto", "compression mode: auto|aggressive|entropy|terse|off")
	ext := fs.String("ext", "", "file-extension hint for language-aware passes (default: inferred from <file>; e.g. .go)")
	format := fs.String("format", "text", "output format: text|json (json reports token savings)")
	out := fs.String("out", "", "write compressed output to FILE instead of stdout")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	switch *mode {
	case "auto", "aggressive", "entropy", "terse", "off":
	default:
		return fmt.Errorf("invalid --mode=%s (want auto|aggressive|entropy|terse|off)", *mode)
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("compress needs exactly one <file|-> argument (use '-' for stdin)")
	}

	src := rest[0]
	var (
		content string
		fileExt = *ext
	)
	if src == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		content = string(data)
	} else {
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		content = string(data)
		if fileExt == "" {
			fileExt = filepath.Ext(src)
		}
	}

	compressed := applyCompressMode(*mode, content, fileExt)

	if *format == "json" {
		rep := struct {
			Mode           string `json:"mode"`
			OriginalBytes  int    `json:"original_bytes"`
			OutputBytes    int    `json:"output_bytes"`
			OriginalTokens int    `json:"original_tokens"`
			OutputTokens   int    `json:"output_tokens"`
			SavedPct       int    `json:"saved_pct"`
			Compressed     string `json:"compressed"`
		}{
			Mode:           *mode,
			OriginalBytes:  len(content),
			OutputBytes:    len(compressed),
			OriginalTokens: tokens.Count(content),
			OutputTokens:   tokens.Count(compressed),
			Compressed:     compressed,
		}
		if rep.OriginalTokens > 0 {
			rep.SavedPct = (rep.OriginalTokens - rep.OutputTokens) * 100 / rep.OriginalTokens
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(compressed), 0o644); err != nil { //nolint:gosec // user-chosen output path
			return fmt.Errorf("write %s: %w", *out, err)
		}
		ot, ct := tokens.Count(content), tokens.Count(compressed)
		saved := 0
		if ot > 0 {
			saved = (ot - ct) * 100 / ot
		}
		fmt.Fprintf(os.Stderr, "✓ %s → %s  (%d → %d tokens, %d%% saved)\n", src, *out, ot, ct, saved)
		return nil
	}
	_, err := fmt.Fprint(os.Stdout, compressed)
	return err
}

// applyCompressMode runs the selected compression mode over content. ext is the
// file-extension hint (with or without leading dot) for language-aware passes.
// Modes that find no improvement fall back to the original content.
func applyCompressMode(mode, content, ext string) string {
	switch mode {
	case "off":
		return content
	case "aggressive", "auto":
		// AggressiveCompress self-degrades to the original via SafeguardRatio
		// when compression would not help, so it is the safe default for auto.
		return compress.AggressiveCompress(content, ext)
	case "entropy":
		lines := strings.Split(content, "\n")
		filtered := compress.EntropyFilter(lines, compress.EntropyThresholdStandard)
		if filtered == nil {
			return content // quality gate rejected — no improvement
		}
		return strings.Join(filtered, "\n")
	case "terse":
		res := compress.TerseCompress(content, compress.Level3)
		return res.Output
	default:
		return content
	}
}

func cmdCompressStdin(args []string) error {
	fs := flag.NewFlagSet("compress-stdin", flag.ContinueOnError)
	command := fs.String("command", "", "command hint (e.g. 'go test', 'git diff') — selects compression patterns")
	maxLines := fs.Int("max-lines", 200, "hard cap on output lines")
	raw := fs.Bool("raw", false, "passthrough — bypass compression")
	if err := fs.Parse(args); err != nil {
		return err
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if *raw {
		_, err = os.Stdout.Write(input)
		return err
	}

	compressed, _, _ := mcp.CompressText(string(input), *command, *maxLines)
	_, err = fmt.Fprint(os.Stdout, compressed)
	return err
}

// shellHookScript is the eval-able bash/zsh hook emitted by `dex shell-hook`.
// It wraps common commands so their output is piped through `dex compress-stdin`,
// reducing token cost when an agent captures command output. Requires dex to be
// on PATH. The user adds `eval "$(dex shell-hook)"` to their shell profile.
const shellHookScript = `# dex shell hook — pipe high-volume commands through dex compress-stdin.
# Add to ~/.bashrc or ~/.zshrc:  eval "$(dex shell-hook)"
_dex_run() {
  local cmd="$1"
  shift
  command "$cmd" "$@" 2>&1 | dex compress-stdin --command "$cmd"
}
# Tool-driver commands: aliasing is safe because their output is consumed by a
# human/agent, not captured in scripts. Core unix utilities (grep/find/rg/ls)
# are deliberately NOT aliased — wrapping them merges stderr and clobbers exit
# codes, breaking 'grep -q', "$(grep …)" capture, and pipelines. Those are
# handled surgically by 'dex hook rewrite' instead.
alias git='_dex_run git'
alias go='_dex_run go'
alias cargo='_dex_run cargo'
alias npm='_dex_run npm'
alias yarn='_dex_run yarn'
alias bun='_dex_run bun'
alias pnpm='_dex_run pnpm'
alias docker='_dex_run docker'
alias kubectl='_dex_run kubectl'
alias make='_dex_run make'
alias gmake='_dex_run gmake'
alias gh='_dex_run gh'
alias pip='_dex_run pip'
alias pip3='_dex_run pip3'
alias uv='_dex_run uv'
alias terraform='_dex_run terraform'
alias tofu='_dex_run tofu'
alias cmake='_dex_run cmake'
alias ninja='_dex_run ninja'
alias eslint='_dex_run eslint'
alias biome='_dex_run biome'
alias ruff='_dex_run ruff'
alias mypy='_dex_run mypy'
alias pytest='_dex_run pytest'
alias tsc='_dex_run tsc'
`

func cmdShellHook(_ []string) error {
	_, err := fmt.Fprint(os.Stdout, strings.TrimLeft(shellHookScript, "\n"))
	return err
}
