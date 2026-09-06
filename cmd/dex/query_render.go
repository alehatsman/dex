package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/mcp"
)

// renderQueryText prints the human view of a query envelope, dispatched by
// route.lane. Each branch reuses an existing lane-specific printer where the
// wire type is unchanged from the former per-verb tool (locate/review); the
// rest (read/grep/trace/semantic/orient/check/refs/cohort/deps/status/select/
// since) are new, minimal text views over the query envelope's flat result.
func renderQueryText(out mcp.QueryOutput) {
	if out.Status != "ok" && out.Result.Check == nil {
		fmt.Fprintf(os.Stderr, "status: %s\n", out.Status)
		if out.Hint != "" {
			fmt.Fprintf(os.Stderr, "hint: %s\n", out.Hint)
		}
		if out.Status == "use-native-read" {
			printQueryNext(out.Next)
		}
		return
	}
	r := out.Result
	switch {
	case r.Read != nil:
		printQueryRead(r.Read)
	case r.Grep != nil:
		printQueryGrep(r.Grep)
	case r.Trace != nil:
		printQueryTrace(r.Trace)
	case r.Locate != nil:
		renderLocateText(*r.Locate)
	case r.Semantic != nil:
		printQuerySemantic(r.Semantic)
	case r.Orient != nil:
		printQueryOrient(r.Orient)
	case r.Review != nil:
		renderReviewText(*r.Review)
	case r.Check != nil:
		printQueryCheck(r.Check)
	case r.Xref != nil:
		printQueryXref(r.Xref)
	case r.Cohort != nil:
		printQueryCohort(r.Cohort)
	case r.Deps != nil:
		printQueryDeps(r.Deps)
	case r.StatusReport != nil:
		printQueryStatus(r.StatusReport)
	case r.Select != nil:
		printQueryRefs(r.Select.Symbols)
	case r.Since != nil:
		printQueryRefs(r.Since.Symbols)
	default:
		fmt.Printf("status: %s (no result payload for lane %q)\n", out.Status, out.Route.Lane)
	}
	printQueryNext(out.Next)
}

func printQueryNext(next []mcp.NextStep) {
	for _, n := range next {
		fmt.Printf("next: %s %v — %s\n", n.Verb, n.Args, n.Why)
	}
}

// ─── read ───────────────────────────────────────────────────────────────

func printQueryRead(ro *mcp.SummarizeOutput) {
	fmt.Printf("file:  %s (", ro.Path)
	if ro.StartLine > 0 || ro.EndLine > 0 {
		fmt.Printf("lines %d-%d, ", ro.StartLine, ro.EndLine)
	}
	fmt.Printf("%d bytes", ro.Bytes)
	if ro.Truncated {
		fmt.Print(", truncated")
	}
	fmt.Println(")")
	// A status "ok" hint means a graceful degrade, not a hard error (e.g.
	// mode=summary falling back to raw content on a chat failure, #854) — it
	// must still reach the reader, or the degrade looks indistinguishable
	// from the real thing.
	if ro.Hint != "" {
		fmt.Printf("hint:  %s\n", ro.Hint)
	}
	// mode=analyze returns a token-cost comparison instead of file content
	// (#623) — printing the (empty) Content field silently dropped it (#854).
	if ro.Analysis != nil {
		printReadAnalysis(ro.Analysis)
		return
	}
	fmt.Println()
	fmt.Println(ro.Content)
}

func printReadAnalysis(a *mcp.ReadAnalysis) {
	fmt.Printf("  %d lines, entropy %.2f bits/char, compressibility: %s\n",
		a.Lines, a.MeanBitsPerChar, a.Compressibility)
	if !a.Indexed {
		fmt.Println("  (unindexed: structural modes unavailable)")
	}
	fmt.Println()
	fmt.Println("  mode         tokens  saved  lossy  note")
	for _, m := range a.Modes {
		lossy := ""
		if m.Lossy {
			lossy = "yes"
		}
		fmt.Printf("  %-12s %6d  %4d%%  %-5s  %s\n", m.Mode, m.Tokens, m.SavedPct, lossy, m.Note)
	}
	fmt.Println()
	fmt.Printf("  recommendation: %s\n", a.Recommendation)
	if a.Reason != "" {
		fmt.Printf("  reason: %s\n", a.Reason)
	}
}

// ─── grep ───────────────────────────────────────────────────────────────

func printQueryGrep(gr *mcp.SearchGrepOutput) {
	for _, m := range gr.Matches {
		if len(m.Before) == 0 && len(m.After) == 0 {
			fmt.Printf("%s:%d: %s\n", m.Path, m.Line, m.Content)
			continue
		}
		for j, l := range m.Before {
			fmt.Printf("%s-%d- %s\n", m.Path, m.Line-len(m.Before)+j, l)
		}
		fmt.Printf("%s:%d: %s\n", m.Path, m.Line, m.Content)
		for j, l := range m.After {
			fmt.Printf("%s-%d- %s\n", m.Path, m.Line+1+j, l)
		}
		fmt.Println("--")
	}
	if gr.Truncated {
		fmt.Fprintf(os.Stderr, "\n(%d matches, truncated)\n", gr.Total)
	}
}

// ─── trace (symbol lane: callers/callees/impact/path) ───────────────────

func printQueryTrace(to *mcp.TraceOutput) {
	switch {
	case len(to.Path) > 0:
		fmt.Printf("path %s → %s (%d hop(s)):\n", to.Src, to.Dst, len(to.Path))
		for i, h := range to.Path {
			fmt.Printf("  %d. %s (%s) %s:%d\n", i, h.QualifiedName, h.Kind, h.Path, h.StartLine)
		}
	case len(to.Nodes) > 0:
		fmt.Printf("blast-radius — %d reachable node(s) within depth %d", to.Total, to.MaxDepth)
		if to.Truncated {
			fmt.Print(" (truncated)")
		}
		fmt.Print(":\n\n")
		for _, n := range to.Nodes {
			fmt.Printf("  d%d  %s (%s) %s:%d\n", n.Depth, n.QualifiedName, n.Kind, n.Path, n.StartLine)
		}
	default:
		fmt.Printf("%s — %d hit(s):\n", to.Direction, len(to.Hits))
		for _, h := range to.Hits {
			loc := h.Path
			if h.CallSiteLine > 0 {
				loc = fmt.Sprintf("%s:%d", h.CallSitePath, h.CallSiteLine)
			}
			fmt.Printf("  %s  (%s)  %s\n", h.QualifiedName, h.Kind, loc)
		}
	}
	if to.Risk != "" {
		fmt.Printf("risk: %s\n", to.Risk)
	}
	if to.Recall == "partial" {
		fmt.Println("recall: partial (name-based recall — non-Go extractor)")
	}
}

// ─── semantic (search/editing/assemble/architecture/packages) ───────────

func printQuerySemantic(sr *mcp.SemanticResult) {
	fmt.Printf("intent: %s  project: %s\n\n", sr.Intent, sr.Project)
	if sr.Answer != "" {
		if sr.AnswerModel != "" {
			fmt.Printf("answer (%s):\n", sr.AnswerModel)
		}
		fmt.Printf("%s\n\n", sr.Answer)
	}
	printSuggestedReads(sr.SuggestedReads, 1500)
	printSymbols(sr.Symbols)
	printReferences(sr.References)
	printAnnotations(sr.Annotations)
	printSemanticHits(sr.SemanticHits)
	printGraph(sr.Graph)
	printRelatedFiles(sr.RelatedFiles)
	printConcerns(sr.Concerns)
	if sr.NextAction != "" {
		fmt.Printf("Next action:\n  %s\n\n", sr.NextAction)
	}
	if sr.Avoid != "" {
		fmt.Printf("Avoid:\n  %s\n", sr.Avoid)
	}
}

// ─── orient ───────────────────────────────────────────────────────────

func printQueryOrient(or *mcp.OrientResult) {
	fmt.Print(or.Map)
	if or.NextAction != "" {
		fmt.Printf("\nNext action:\n  %s\n", or.NextAction)
	}
	if or.Avoid != "" {
		fmt.Printf("\nAvoid:\n  %s\n", or.Avoid)
	}
}

// ─── check ───────────────────────────────────────────────────────────

func printQueryCheck(co *mcp.CheckOutput) {
	for _, r := range co.Results {
		switch r.Status {
		case "ok":
			sym := r.SymbolAt
			if sym == "" {
				sym = "(file indexed)"
			}
			fmt.Printf("ok      %s  — %s\n", r.Ref, sym)
		case "moved":
			fmt.Printf("moved   %s  → %s\n", r.Ref, r.FoundAt)
		case "gone":
			fmt.Printf("gone    %s\n", r.Ref)
		case "no_file":
			fmt.Printf("no_file %s\n", r.Ref)
		case "parse_error":
			fmt.Printf("parse?  %s\n", r.Ref)
		default:
			fmt.Printf("%-8s %s\n", strings.TrimRight(r.Status, " "), r.Ref)
		}
	}
}

// ─── refs (xref: references/implementations/supertypes/subtypes) ───────

func printQueryXref(ro *mcp.RefsOutput) {
	if len(ro.Sites) == 0 {
		fmt.Printf("refs %s %s — no results\n", ro.Action, ro.Symbol)
		return
	}
	fmt.Printf("refs %s %s — %d result(s):\n", ro.Action, ro.Symbol, len(ro.Sites))
	for _, s := range ro.Sites {
		fmt.Printf("  %s:%d  [%s]\n", s.Path, s.Line, s.Kind)
	}
}

// ─── cohort ───────────────────────────────────────────────────────────

func printQueryCohort(co *mcp.CohortOutput) {
	fmt.Printf("cohort of %s (%d methods) — %d complete, %d partial:\n",
		co.Interface, len(co.Methods), co.Complete, co.Partial)
	for _, m := range co.Members {
		loc := fmt.Sprintf("%s:%d", m.Path, m.Line)
		if m.Status == "complete" {
			fmt.Printf("  ✓ %s  %s\n", m.Type, loc)
		} else {
			fmt.Printf("  ✗ %s  %s  — missing: %v\n", m.Type, loc, m.Missing)
		}
	}
}

// ─── deps ───────────────────────────────────────────────────────────

func printQueryDeps(do *mcp.GraphDepsOutput) {
	fmt.Printf("package: %s\n", do.Package)
	if len(do.Imports) == 0 {
		fmt.Println("(no import edges)")
		return
	}
	for _, dep := range do.Imports {
		fmt.Printf("  → %s\n", dep.ToPackage)
	}
}

// ─── status ───────────────────────────────────────────────────────────

func printQueryStatus(so *mcp.StatusOutput) {
	fmt.Printf("dex %s · %s\n", so.Version, so.IndexDir)
	fmt.Printf("embed:  %s  reachable=%v  model=%s\n", so.Endpoint, so.Reachable, so.Model)
	if so.ChatEndpoint != "" {
		fmt.Printf("chat:   %s  reachable=%v  model=%s\n", so.ChatEndpoint, so.ChatReachable, so.ChatModel)
	}
	if so.RerankEndpoint != "" {
		fmt.Printf("rerank: %s  reachable=%v  model=%s\n", so.RerankEndpoint, so.RerankReachable, so.RerankModel)
	}
	for _, p := range so.Projects {
		fmt.Printf("  %s  chunks=%d files=%d\n", p.Root, p.Chunks, p.Files)
	}
}

// ─── select / since (a `field:pattern` or `since:<ref>` seed) ───────────

func printQueryRefs(refs []mcp.Ref) {
	fmt.Printf("%d symbol(s):\n", len(refs))
	for _, r := range refs {
		fmt.Printf("  %s  (%s)\n", r.ID, r.Kind)
	}
}

// ─── locate (ported from the former cmd/dex/locate.go) ──────────────────

// renderLocateText prints the human view of a locate bundle.
func renderLocateText(out mcp.LocateOutput) {
	loc := out.Path
	if out.StartLine > 0 {
		loc = fmt.Sprintf("%s:%d-%d", out.Path, out.StartLine, out.EndLine)
	}
	fmt.Printf("%s (%s)  %s", out.Symbol, out.Kind, loc)
	if out.Risk != "" {
		fmt.Printf("   [risk: %s]", out.Risk)
	}
	fmt.Println()
	if len(out.Callers) > 0 {
		fmt.Printf("  callers (%d):\n", len(out.Callers))
		for _, c := range out.Callers {
			cloc := c.Path
			if c.CallSiteLine > 0 {
				cloc = fmt.Sprintf("%s:%d", c.CallSitePath, c.CallSiteLine)
			}
			fmt.Printf("    %s  (%s)  %s\n", c.QualifiedName, c.Kind, cloc)
		}
	}
	if len(out.Tests) > 0 {
		fmt.Printf("  tests: %v\n", out.Tests)
	}
	if out.NearestDoc != "" {
		fmt.Printf("  doc: %s\n", out.NearestDoc)
	}
	if out.LastCommit != "" {
		fmt.Printf("  last: %s  (%s)\n", out.LastCommit, out.LastAuthor)
	}
	if len(out.Issues) > 0 {
		fmt.Printf("  issues:\n")
		for _, is := range out.Issues {
			fmt.Printf("    %s\n", is)
		}
	}
	if out.Hint != "" {
		fmt.Printf("  hint: %s\n", out.Hint)
	}
}

// ─── review (ported from the former cmd/dex/review.go) ──────────────────

// renderReviewText prints the human view of a review bundle.
func renderReviewText(out mcp.ReviewOutput) {
	fmt.Printf("review of %s — %d hunk(s) across %d file(s)", out.Range, out.TotalHunks, len(out.Files))
	if out.Truncated {
		fmt.Print(" (truncated)")
	}
	fmt.Print(":\n")
	for _, f := range out.Files {
		fmt.Printf("\n─── %s (%s)", f.Path, f.Status)
		if f.OldPath != "" {
			fmt.Printf("  ⟵ %s", f.OldPath)
		}
		fmt.Println()
		if f.LastCommit != "" {
			fmt.Printf("    last: %s  (%s)\n", f.LastCommit, f.LastAuthor)
		}
		if f.Churn30d > 0 {
			fmt.Printf("    churn(30d): %d commits", f.Churn30d)
			if len(f.AuthorHistory) > 0 {
				fmt.Printf("  authors: %v", f.AuthorHistory)
			}
			fmt.Println()
		}
		if len(f.Tests) > 0 {
			fmt.Printf("    tests: %v\n", f.Tests)
		}
		if f.NearestDoc != "" {
			fmt.Printf("    doc: %s\n", f.NearestDoc)
		}
		for _, h := range f.Hunks {
			fmt.Printf("    @@ %d,%d → %d,%d  [%s] %s\n",
				h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.RiskTier, h.RiskReason)
			if h.Heading != "" {
				fmt.Printf("       in: %s\n", h.Heading)
			}
			for _, sym := range h.SymbolsTouched {
				exp := ""
				if sym.Exported {
					exp = " (exported)"
				}
				callers := ""
				if sym.CallerCount > 0 {
					callers = fmt.Sprintf(" — %d callers", sym.CallerCount)
				}
				fmt.Printf("       • %s%s%s\n", sym.Name, exp, callers)
			}
		}
	}
}
