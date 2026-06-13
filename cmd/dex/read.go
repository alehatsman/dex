package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
)

// cmdRead is the `dex read <file>` verb (MCP: read). It shares the MCP tool's
// mode vocabulary so an agent and a human read with one verb:
//
//   - full        — raw content, no LLM (the default; honors --start/--end)
//   - signatures  — index-backed declaration view (imports + top-level decls,
//     bodies dropped); falls back to aggressive when the file is
//     not indexed or has no symbols
//   - aggressive  — internal/compress AggressiveCompress (comments/structure)
//   - entropy     — drop low-information lines
//   - auto        — large indexed files → signatures, otherwise full (mirrors
//     the redirect hook's redirectLineThreshold)
//   - summary     — LLM-generated digest (the only mode that calls the chat
//     model; honors --focus/--temperature/--max-tokens)
//
// `dex compress` (#229) is the generic text-in/out sibling; `dex read` is
// file-oriented with line ranges and index-aware mode auto-selection.
func cmdRead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	setHelp(fs,
		"Read a file (MCP: read). Default mode=full is raw content (no LLM); mode=summary is an LLM digest.",
		"dex read [flags] <file>",
		`dex read internal/store/store.go`,
		`dex read --mode=signatures internal/mcp/server.go`,
		`dex read --start=100 --end=160 cmd/dex/main.go`,
		`dex read --mode=summary --focus="public API" internal/mcp/server.go`,
	)
	mode := fs.String("mode", "full", "read mode: full|signatures|aggressive|entropy|auto|summary")
	start := fs.Int("start", 0, "first line to read (1-based, inclusive; 0 = file start)")
	end := fs.Int("end", 0, "last line to read (1-based, inclusive; 0 = end of file)")
	format := fs.String("format", "text", "output format: text|json")
	focus := fs.String("focus", "", "summary mode: steer the digest — e.g. 'public API surface'")
	temp := fs.Float64("temperature", 0, "summary mode: sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "summary mode: max tokens to generate (0 = server default)")
	verbose := fs.Bool("v", false, "summary mode: include model name in text output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	switch *mode {
	case "auto", "full", "signatures", "aggressive", "entropy", "summary":
	default:
		return fmt.Errorf("invalid --mode=%s (want full|signatures|aggressive|entropy|auto|summary)", *mode)
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("read needs exactly one <file> argument")
	}
	path := rest[0]
	if *mode == "summary" {
		return readSummarize(ctx, path, *start, *end, *focus, *temp, *maxTok, *format, *verbose)
	}
	if *start < 0 || *end < 0 {
		return fmt.Errorf("--start/--end must be non-negative")
	}
	if *start > 0 && *end > 0 && *end < *start {
		return fmt.Errorf("--end (%d) is before --start (%d)", *end, *start)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	fullLines := strings.Split(string(data), "\n")
	ext := filepath.Ext(path)
	// Weak target_model profiles get the anchor-verbatim floor (#291): paths,
	// qualified identifiers, and type names survive compression byte-identical.
	strict := profiles.Active(filepath.Dir(path)).StrictAnchors()

	resolved := *mode
	var content string

	switch *mode {
	case "signatures":
		if view := buildSignaturesView(ctx, path, fullLines); view != "" {
			content = view
		} else {
			// Not indexed / no symbols — degrade to a structural compress
			// rather than failing, and say so on stderr (text mode only).
			if *format == "text" {
				fmt.Fprintln(os.Stderr, "dex read: no index/symbols for this file — falling back to --mode=aggressive")
			}
			resolved = "aggressive"
			content = compress.CompressCode(rangeText(fullLines, *start, *end), ext, strict)
		}
	case "auto":
		// Mirror the redirect hook: large indexed files get a signatures view;
		// otherwise emit the (possibly ranged) full content.
		if *start == 0 && *end == 0 && len(fullLines) >= redirectLineThreshold {
			if view := buildSignaturesView(ctx, path, fullLines); view != "" {
				resolved = "signatures"
				content = view
				break
			}
		}
		resolved = "full"
		content = rangeText(fullLines, *start, *end)
	case "full":
		content = rangeText(fullLines, *start, *end)
	case "aggressive":
		content = compress.CompressCode(rangeText(fullLines, *start, *end), ext, strict)
	case "entropy":
		ranged := strings.Split(rangeText(fullLines, *start, *end), "\n")
		filtered := compress.EntropyFilter(ranged, compress.EntropyThresholdStandard)
		if filtered == nil {
			content = strings.Join(ranged, "\n")
		} else {
			content = strings.Join(filtered, "\n")
		}
	}

	if *format == "json" {
		rep := struct {
			Path        string `json:"path"`
			Mode        string `json:"mode"`
			Start       int    `json:"start,omitempty"`
			End         int    `json:"end,omitempty"`
			TotalLines  int    `json:"total_lines"`
			OutputLines int    `json:"output_lines"`
			Content     string `json:"content"`
		}{
			Path:        path,
			Mode:        resolved,
			Start:       *start,
			End:         *end,
			TotalLines:  len(fullLines),
			OutputLines: strings.Count(content, "\n") + 1,
			Content:     content,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	_, err = fmt.Fprint(os.Stdout, content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Fprintln(os.Stdout)
	}
	return err
}

// readSummarize handles `dex read --mode=summary`: the LLM digest path (MCP:
// read mode=summary). It routes through the same Summarize handler the MCP
// `read` tool uses, so CLI and tool produce the same digest. Returns a
// needs-chat status (not an error) when no chat model is wired.
func readSummarize(ctx context.Context, file string, start, end int, focus string, temp float64, maxTok int, format string, verbose bool) error {
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve("", base)
	if err != nil {
		return err
	}
	s, _ := newServerFromEnv(base)
	out, err := s.Summarize(ctx, mcp.SummarizeInput{
		Path:        file,
		ProjectRoot: p.Root,
		StartLine:   start,
		EndLine:     end,
		Focus:       focus,
		Temperature: float32(temp),
		MaxTokens:   maxTok,
		Mode:        "summary",
	})
	if err != nil {
		return err
	}
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if out.Status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
		}
		return nil
	}
	fmt.Printf("file:  %s (lines %d-%d, %d bytes", out.Path, out.StartLine, out.EndLine, out.Bytes)
	if out.Truncated {
		fmt.Print(", truncated")
	}
	fmt.Println(")")
	if verbose && out.Model != "" {
		fmt.Printf("model: %s\n", out.Model)
	}
	fmt.Println()
	fmt.Println(out.Content)
	if out.FinishReason != "" && out.FinishReason != "stop" {
		fmt.Fprintf(os.Stderr, "\n(finish_reason=%s)\n", out.FinishReason)
	}
	return nil
}

// rangeText returns the [start,end] (1-based, inclusive) slice of lines joined
// by newlines. start<=0 means "from the first line"; end<=0 or beyond the file
// means "to the last line". Out-of-range starts yield an empty string.
func rangeText(lines []string, start, end int) string {
	if start <= 0 && end <= 0 {
		return strings.Join(lines, "\n")
	}
	lo := start
	if lo <= 0 {
		lo = 1
	}
	hi := end
	if hi <= 0 || hi > len(lines) {
		hi = len(lines)
	}
	if lo > len(lines) || lo > hi {
		return ""
	}
	return strings.Join(lines[lo-1:hi], "\n")
}
