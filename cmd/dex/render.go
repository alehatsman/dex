// Output rendering for the CLI subcommands. Splitting these out keeps
// main.go focused on dispatch + env wiring, and makes it obvious which
// pieces are "presentation only" vs "real work".
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
)

// truncate clips s to n bytes (n=0 means no limit), snapping back to a UTF-8
// boundary so we don't emit a half-rune sequence to the terminal.
// The truncation indicator names the byte limit so users know what to pass
// to --max-content-bytes to see more.
func truncate(s string, n int) string {
	if n == 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…(truncated at %d bytes; pass --max-content-bytes to override)", n)
}

// relativeTime formats a timestamp as a human-friendly relative string
// ("just now", "5m ago", "2h ago", "3d ago", or a date for old entries).
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// printSearchHitResult renders the shared status/hint/hits shape used
// by the search-style MCP tools (search_semantic, search_symbol,
// graph_neighbors). Single helper keeps the CLI's text output for all
// three surfaces visually identical.
// maxBytes controls content truncation (0 = no limit; 1500 is the default
// that callers pass when no --max-content-bytes flag is set).
func printSearchHitResult(status, hint, project string, hits []mcp.SearchHit, maxBytes int) {
	if status != "" && status != "ok" {
		fmt.Fprintf(os.Stderr, "status: %s\n", status)
		if hint != "" {
			fmt.Fprintf(os.Stderr, "hint:   %s\n", hint)
		}
		return
	}
	if project != "" {
		fmt.Printf("project: %s\n", project)
	}
	if hint != "" {
		fmt.Printf("hint: %s\n", hint)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return
	}
	for i, h := range hits {
		loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
		header := fmt.Sprintf("─── #%d %s  (%s)  score=%.4f", i+1, loc, h.Kind, h.Score)
		if h.RerankScore > 0 {
			header += fmt.Sprintf("  rerank=%.4f", h.RerankScore)
		}
		if h.Role != "" {
			header += "  role=" + h.Role
		}
		fmt.Println(header)
		if h.Content != "" {
			fmt.Println(truncate(h.Content, maxBytes))
		}
		fmt.Println()
	}
}

// printContextText emits a human-readable rendering of a ContextOutput.
// maxBytes limits content display (0 = no limit). Mirrors the layout
// cmdQuery uses for hits so the two surfaces feel like the same tool.
// printContextText renders the bundle. When answerHandled is true the
// answer was already streamed to stdout by the caller, so the answer
// block is skipped here to avoid printing it twice.
func printContextText(out mcp.ContextOutput, maxBytes int, answerHandled bool) {
	if out.Status != "ok" {
		printContextError(out)
		return
	}
	printContextHeader(out)
	if !answerHandled && out.Answer != "" {
		if out.AnswerModel != "" {
			fmt.Printf("answer (%s):\n", out.AnswerModel)
		}
		fmt.Printf("%s\n\n", out.Answer)
	}
	printSuggestedReads(out.SuggestedReads, maxBytes)
	printSymbols(out.Symbols)
	printReferences(out.References)
	printAnnotations(out.Annotations)
	printSemanticHits(out.SemanticHits)
	printGraph(out.Graph)
	printRelatedFiles(out.RelatedFiles)
	printConcerns(out.Concerns)
	printNextActionAndAvoid(out)
}

// printConcerns renders the assemble completeness signal (#725): which query
// concerns the working set covers vs which the byte budget dropped.
func printConcerns(c *mcp.AssembleConcerns) {
	if c == nil || (len(c.Covered) == 0 && len(c.Dropped) == 0) {
		return
	}
	fmt.Printf("Concerns: covered %d, dropped %d\n", len(c.Covered), len(c.Dropped))
	if len(c.Dropped) > 0 {
		fmt.Printf("  ⚠ dropped (no symbol body in the set): %s\n", strings.Join(c.Dropped, ", "))
	}
	fmt.Println()
}

func printContextError(out mcp.ContextOutput) {
	fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
	if out.Hint != "" {
		fmt.Fprintf(os.Stderr, "hint:   %s\n", out.Hint)
	}
	if out.Endpoint != "" {
		fmt.Fprintf(os.Stderr, "endpoint: %s\n", out.Endpoint)
	}
}

func printContextHeader(out mcp.ContextOutput) {
	fmt.Printf("intent: %s  project: %s\n", out.Intent, out.Project)
	if out.Trust != nil && out.Trust.Fresh != nil && !*out.Trust.Fresh {
		fmt.Println("⚠ index is stale — refresh recommended")
	}
	if out.Hint != "" {
		fmt.Printf("hint: %s\n", out.Hint)
	}
	fmt.Println()
}

func printSuggestedReads(reads []mcp.SuggestedRead, maxBytes int) {
	if len(reads) == 0 {
		return
	}
	fmt.Println("Suggested reads:")
	for i, r := range reads {
		loc := r.Path
		if r.StartLine > 0 || r.EndLine > 0 {
			loc = fmt.Sprintf("%s:%d-%d", r.Path, r.StartLine, r.EndLine)
		}
		fmt.Printf("  %d. %s\n     reason: %s\n", i+1, loc, r.Reason)
		if r.Content != "" {
			body := truncate(r.Content, maxBytes)
			for line := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
				fmt.Printf("     │ %s\n", line)
			}
			if r.Truncated && maxBytes == 0 {
				fmt.Println("     │ … (truncated; Read the file for the rest)")
			}
		}
	}
	fmt.Println()
}

func printSymbols(symbols []mcp.SymbolHit) {
	if len(symbols) == 0 {
		return
	}
	fmt.Println("Relevant symbols:")
	for _, sym := range symbols {
		loc := sym.Path
		if sym.StartLine > 0 {
			loc = fmt.Sprintf("%s:%d", sym.Path, sym.StartLine)
		}
		fmt.Printf("  - %s  (%s)  %s\n", sym.QualifiedName, sym.Kind, loc)
		if sym.Signature != "" {
			fmt.Printf("      sig: %s\n", sym.Signature)
		}
		if sym.Doc != "" {
			for line := range strings.SplitSeq(sym.Doc, "\n") {
				fmt.Printf("      doc: %s\n", line)
			}
		}
	}
	fmt.Println()
}

func printRelatedFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Println("Related files:")
	for _, p := range paths {
		fmt.Printf("  %s\n", p)
	}
	fmt.Println()
}

func printReferences(refs []mcp.RefHit) {
	if len(refs) == 0 {
		return
	}
	fmt.Println("References:")
	for _, r := range refs {
		fmt.Printf("  - %s:%d  %s\n", r.Path, r.Line, r.Snippet)
	}
	fmt.Println()
}

func printAnnotations(anns map[string]mcp.PathMeta) {
	if len(anns) == 0 {
		return
	}
	fmt.Println("Annotations:")
	for path, meta := range anns {
		fmt.Printf("  %s\n", path)
		if meta.LastCommit != "" {
			fmt.Printf("    last:    %s  %s\n", meta.LastCommit, meta.LastAuthor)
		}
		if len(meta.Owners) > 0 {
			fmt.Printf("    owners:  %s\n", strings.Join(meta.Owners, " "))
		}
		if meta.NearestDoc != "" {
			fmt.Printf("    doc:     %s\n", meta.NearestDoc)
		}
		if len(meta.Tests) > 0 {
			fmt.Printf("    tests:   %s\n", strings.Join(meta.Tests, " "))
		}
		if meta.BuildTags != "" {
			fmt.Printf("    build:   %s\n", meta.BuildTags)
		}
		if meta.Package != "" {
			fmt.Printf("    package: %s\n", meta.Package)
		}
	}
	fmt.Println()
}

func printSemanticHits(hits []mcp.SemHit) {
	if len(hits) == 0 {
		return
	}
	fmt.Println("Semantic hits:")
	for i, h := range hits {
		loc := fmt.Sprintf("%s:%d-%d", h.Path, h.StartLine, h.EndLine)
		fmt.Printf("  %d. %s  (%s)  score=%.4f", i+1, loc, h.Kind, h.Score)
		if h.Reason != "" {
			fmt.Printf("  (%s)", h.Reason)
		}
		fmt.Println()
	}
	fmt.Println()
}

func printGraph(gr *mcp.GraphResult) {
	if gr == nil || (len(gr.Nodes) == 0 && len(gr.Edges) == 0) {
		return
	}
	fmt.Println("Graph:")
	for _, n := range gr.Nodes {
		fmt.Printf("  node  %-12s  %s\n", n.Kind, n.ID)
	}
	for _, e := range gr.Edges {
		fmt.Printf("  edge  %-12s  %s → %s\n", e.Kind, e.From, e.To)
	}
	fmt.Println()
}

func printNextActionAndAvoid(out mcp.ContextOutput) {
	if out.NextAction != "" {
		fmt.Printf("Next action:\n  %s\n\n", out.NextAction)
	}
	if out.Avoid != "" {
		fmt.Printf("Avoid:\n  %s\n", out.Avoid)
	}
}
