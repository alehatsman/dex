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
//   - skeleton    — exported type decls in full + function/method signatures
//     with @B<n> body handles, no LLM (delegates to the server)
//   - map         — imports + exported symbols from the index, no LLM
//     (delegates to the server)
//   - summary     — LLM-generated digest (the only mode that calls the chat
//     model; honors --focus/--temperature/--max-tokens)
//
// `entropy`/`auto` are CLI-local conveniences; the MCP `read` tool's
// session-scoped extras (`expand` body handles, `handle` budget downgrade) are
// MCP-only by nature — handles live in per-session server memory, so a separate
// CLI process can't resolve a handle issued by a prior one. See verbs parity
// test read_parity_test.go.
//
// `dex compress` (#229) is the generic text-in/out sibling; `dex read` is
// file-oriented with line ranges and index-aware mode auto-selection.
func cmdRead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	setHelp(fs,
		"Read a file (MCP: read). Default mode=full is raw content (no LLM); mode=summary is an LLM digest.",
		"dex read [flags] [<path>] <file>",
		`dex read internal/store/store.go`,
		`dex read --mode=signatures internal/mcp/server.go`,
		`dex read --start=100 --end=160 cmd/dex/main.go`,
		`dex read --mode=summary --focus="public API" internal/mcp/server.go`,
	)
	mode := fs.String("mode", "full", "read mode: "+strings.Join(readModeChoices, "|"))
	start := fs.Int("start", 0, "first line to read (1-based, inclusive; 0 = file start)")
	end := fs.Int("end", 0, "last line to read (1-based, inclusive; 0 = end of file)")
	format := fs.String("format", "text", "output format: text|json")
	focus := fs.String("focus", "", "summary mode: steer the digest — e.g. 'public API surface'")
	temp := fs.Float64("temperature", 0, "summary mode: sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "summary mode: max tokens to generate (0 = server default)")
	handle := fs.String("handle", "", "expand an opaque file/range handle (e.g. from `read --mode analyze`); supersedes <file>")
	verbose := fs.Bool("v", false, "summary mode: include model name in text output")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if !validReadMode(*mode) {
		return fmt.Errorf("invalid --mode=%s (want %s)", *mode, strings.Join(readModeChoices, "|"))
	}
	switch *format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown --format=%s (want text|json)", *format)
	}
	projPath, rest := splitProjectArg(fs.Args())
	// A handle carries its own path (#620/#344), so <file> is optional with it;
	// resolution lives server-side (applyExpansionHandle), so always delegate.
	if *handle != "" {
		if len(rest) != 0 {
			return fmt.Errorf("read --handle takes no <file> argument (the handle carries the path)")
		}
		return readViaServer(ctx, projPath, "", *mode, *start, *end, *focus, *temp, *maxTok, *format, *verbose, *handle)
	}
	if len(rest) != 1 {
		return fmt.Errorf("read needs exactly one <file> argument")
	}
	path := rest[0]
	// skeleton/map/summary live in the index + summarize handler — delegate to
	// the same Server.Summarize the MCP `read` tool uses so CLI and tool agree.
	// (The local fast paths below avoid a server spin-up for the hot modes.)
	if serverReadMode(*mode) {
		return readViaServer(ctx, projPath, path, *mode, *start, *end, *focus, *temp, *maxTok, *format, *verbose, "")
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
		// Lossless JSON compaction (#619): whole-file .json/.jsonl reads recover
		// 20–50% whitespace with zero semantic loss. Only for the entire file —
		// a partial line range may slice mid-token and break the scan.
		if *start == 0 && *end == 0 {
			switch strings.ToLower(ext) {
			case ".jsonl", ".ndjson":
				if c, ok := compress.CompactJSONL(content); ok {
					content = c
				}
			case ".json":
				if c, ok := compress.CompactJSON(content); ok {
					content = c
				}
			}
		}
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

// readViaServer handles the index/summarize-backed read modes (skeleton, map,
// summary) by routing through the same Server.Summarize handler the MCP `read`
// tool uses, so the CLI and the tool produce identical output. Focus/temp/
// max-tokens/verbose are summary-only and ignored by the deterministic modes.
// For summary, a missing chat model yields a needs-chat status, not an error.
func readViaServer(ctx context.Context, projPath, file, mode string, start, end int, focus string, temp float64, maxTok int, format string, verbose bool, handle string) error {
	base, err := indexDir()
	if err != nil {
		return err
	}
	p, err := proj.Resolve(projPath, base)
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
		Mode:        mode,
		Handle:      handle,
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
	if out.Analysis != nil {
		printReadAnalysis(out.Analysis)
		return nil
	}
	// Whole-file structural modes (skeleton/map) report no line bounds; only
	// show the range for the line-oriented modes that set one.
	fmt.Printf("file:  %s (", out.Path)
	if out.StartLine > 0 || out.EndLine > 0 {
		fmt.Printf("lines %d-%d, ", out.StartLine, out.EndLine)
	}
	fmt.Printf("%d bytes", out.Bytes)
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

// printReadAnalysis renders a mode=analyze result as a compact text table.
func printReadAnalysis(a *mcp.ReadAnalysis) {
	idx := "no"
	if a.Indexed {
		idx = "yes"
	}
	fmt.Printf("file:  %s (%d lines, %d bytes, indexed=%s)\n", a.Path, a.Lines, a.Bytes, idx)
	fmt.Printf("entropy: %.2f bits/char · compressibility: %s\n\n", a.MeanBitsPerChar, a.Compressibility)
	fmt.Printf("  %-12s %8s  %6s  %s\n", "mode", "tokens", "saved", "")
	for _, m := range a.Modes {
		tag := ""
		if m.Lossy {
			tag = "lossy"
		}
		if m.Note != "" {
			tag = m.Note
		}
		fmt.Printf("  %-12s %8d  %5d%%  %s\n", m.Mode, m.Tokens, m.SavedPct, tag)
	}
	fmt.Printf("\nrecommendation: %s", a.Recommendation)
	if a.Reason != "" {
		fmt.Printf("  — %s", a.Reason)
	}
	fmt.Println()
	if a.Handle != "" {
		fmt.Printf("handle: %s  (expand later: read --handle %s --mode %s)\n", a.Handle, a.Handle, a.Recommendation)
	}
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
