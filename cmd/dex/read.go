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
	"github.com/alehatsman/dex/internal/ignore"
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
		`dex read --ref=HEAD~5 --mode=signatures internal/store/store.go  # the file as of 5 commits ago`,
	)
	mode := fs.String("mode", "full", "read mode: "+strings.Join(readModeChoices, "|"))
	start := fs.Int("start", 0, "first line to read (1-based, inclusive; 0 = file start)")
	end := fs.Int("end", 0, "last line to read (1-based, inclusive; 0 = end of file)")
	format := fs.String("format", "text", "output format: text|json")
	focus := fs.String("focus", "", "summary mode: steer the digest — e.g. 'public API surface'")
	temp := fs.Float64("temperature", 0, "summary mode: sampling temperature (0 = server default)")
	maxTok := fs.Int("max-tokens", 0, "summary mode: max tokens to generate (0 = server default)")
	handle := fs.String("handle", "", "expand an opaque file/range handle (e.g. from `read --mode analyze`); supersedes <file>")
	ref := fs.String("ref", "", "read the file as of a git ref (e.g. HEAD~5, v1.0); supports full/lines/signatures (#644)")
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
	// --ref time-travels the file's content to a git ref (#644). The index-backed
	// modes (skeleton/map/summary) describe HEAD's index, so they can't serve a
	// historical version — reject them with a clear pointer to the content modes.
	if *ref != "" && serverReadMode(*mode) {
		return fmt.Errorf("--ref does not support --mode=%s (it reads the HEAD index); use full, lines, or signatures", *mode)
	}
	// skeleton/map/summary live in the index + summarize handler — delegate to
	// the same Server.Summarize the MCP `read` tool uses so CLI and tool agree.
	// (The local fast paths below avoid a server spin-up for the hot modes.)
	if serverReadMode(*mode) {
		return readViaServer(ctx, projPath, path, *mode, *start, *end, *focus, *temp, *maxTok, *format, *verbose, "")
	}
	if err := validateLineRange(*start, *end); err != nil {
		return err
	}

	data, err := readSource(ctx, path, *ref)
	if err != nil {
		return err
	}
	if done, err := emitBinaryRefusal(path, data, *format); done {
		return err
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
		// --ref reads a historical version, but buildSignaturesView queries the
		// HEAD index — it would return today's symbols for a past file. Skip it
		// and compress the historical content with tree-sitter instead.
		var view string
		if *ref == "" {
			view = buildSignaturesView(ctx, path, fullLines)
		}
		if view != "" {
			content = view
		} else {
			// Not indexed / no symbols (or a --ref read) — degrade to a structural
			// compress rather than failing; note it on stderr for working-tree reads.
			if *format == "text" && *ref == "" {
				fmt.Fprintln(os.Stderr, "dex read: no index/symbols for this file — falling back to --mode=aggressive")
			}
			resolved = "aggressive"
			content = compress.CompressCode(rangeText(fullLines, *start, *end), ext, strict)
		}
	case "auto":
		// Mirror the redirect hook: large indexed files get a signatures view;
		// otherwise emit the (possibly ranged) full content. --ref skips the
		// HEAD-indexed view (see signatures above).
		if *ref == "" && *start == 0 && *end == 0 && len(fullLines) >= redirectLineThreshold {
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
			content = maybeCompactJSON(content, ext)
		}
	case "aggressive":
		content = compress.CompressCode(rangeText(fullLines, *start, *end), ext, strict)
	case "entropy":
		content = entropyFiltered(rangeText(fullLines, *start, *end))
	}

	if *format == "json" {
		rep := readJSONOutput{
			Path:        path,
			Mode:        resolved,
			Start:       *start,
			End:         *end,
			TotalLines:  len(fullLines),
			OutputLines: strings.Count(content, "\n") + 1,
			Content:     content,
			Envelope:    buildReadEnvelope(path, resolved, *start, *end, len(fullLines)),
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

// validateLineRange rejects negative --start/--end and an end that precedes a
// start. Both bounds are 1-based; 0 means "open" (file start / end).
func validateLineRange(start, end int) error {
	if start < 0 || end < 0 {
		return fmt.Errorf("--start/--end must be non-negative")
	}
	if start > 0 && end > 0 && end < start {
		return fmt.Errorf("--end (%d) is before --start (%d)", end, start)
	}
	return nil
}

// emitBinaryRefusal mirrors the index-time binary skip on the read path: rather
// than dumping raw bytes (null bytes included) into the agent's context, it
// prints a one-line refusal and reports done=true (#674). Returns done=false
// for text content so the caller proceeds normally.
func emitBinaryRefusal(path string, data []byte, format string) (bool, error) {
	if !ignore.LooksBinary(data) {
		return false, nil
	}
	hint := fmt.Sprintf("binary file (%d bytes) — not shown; dex does not read binary content", len(data))
	if format == "json" {
		return true, json.NewEncoder(os.Stdout).Encode(map[string]any{
			"path": path, "status": "binary", "bytes": len(data), "hint": hint,
		})
	}
	fmt.Fprintf(os.Stderr, "status: binary\nhint:   %s\n", hint)
	return true, nil
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
