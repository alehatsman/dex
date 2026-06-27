package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/feedback"
)

// `dex feedback` reads the observe log (hooks.jsonl) the PostToolUse hook
// writes and turns it into a relevance signal on real traffic (#724). The
// reader and ask-vs-read join live in internal/feedback (the single in-process
// home, shared with the live reweighter #731); this file is the CLI gauge over
// them.
//
// It reports, per session (segmented by the Stop / PreCompact boundary events
// the hook records):
//
//   - suggested-read open-rate: of the reads `ask` recommended, what fraction
//     the agent opened afterward — broken down by routed intent;
//   - inline-reopen rate: of `ask` calls that inlined content, what fraction
//     were followed by re-opening a recommended file anyway.
//
// The pruned-then-re-read (CCR) rate is already measured proxy-side
// (rereads_after_stub in the proxy request log); see `dex proxy --stats`.

// Type aliases keep the CLI's JSON contract (and #732's --mine-curated) bound
// to the canonical types in internal/feedback.
type (
	feedbackEvent  = feedback.Event
	feedbackReport = feedback.Report
)

func runFeedback(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("feedback", flag.ContinueOnError)
	logPath := fs.String("log", "", "path to hooks.jsonl (default: $XDG_DATA_HOME/dex/hooks.jsonl)")
	window := fs.Int("window", 0, "max consume events to look ahead per suggested read within a session (0 = whole session)")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	mineCurated := fs.Bool("mine-curated", false, "emit GoldenQuery candidates mined from missed asks (JSON array, review before adding to curated.json)")
	projectPath := fs.String("project", ".", "project root — used to make absolute opened paths relative (for --mine-curated)")
	shadow := fs.Bool("shadow", false, "A/B the #731 shadow reweight: join feedback_shadow.jsonl against the observe log and report whether the shadow top-k caught more opened files than served")
	shadowLog := fs.String("shadow-log", "", "path to feedback_shadow.jsonl (default: $XDG_DATA_HOME/dex/feedback_shadow.jsonl)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *logPath
	if path == "" {
		path = filepath.Join(hookLogDir(), "hooks.jsonl")
	}
	events, err := feedback.ReadLog(path)
	if err != nil {
		return err
	}

	if *shadow {
		return runFeedbackShadow(os.Stdout, events, *shadowLog, *window, *asJSON)
	}

	if *mineCurated {
		root, absErr := filepath.Abs(*projectPath)
		if absErr != nil {
			return fmt.Errorf("feedback --mine-curated: resolve project path: %w", absErr)
		}
		candidates := mineCuratedCandidates(events, *window, root)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(candidates)
	}

	rep := feedback.Compute(events, *window)
	rep.LogPath = path

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printFeedbackReport(os.Stdout, rep)
	return nil
}

// mineCuratedCandidates scans the observe log for asks whose suggested_reads
// did NOT include files the agent subsequently opened. Each (query, missed-file)
// pair is a candidate for a new curated golden entry. Candidates are merged
// across sessions by query: the same query missing the same file in multiple
// sessions accumulates a higher session_count, which the JSON output sorts by
// descending so the most-missed pairs appear first for human review.
//
// The output is []eval.GoldenQuery so the caller can paste directly into
// benchmark/eval/curated.json after reviewing. Absolute paths are made
// relative to root; paths that cannot be made relative are kept as-is.
func mineCuratedCandidates(events []feedbackEvent, window int, root string) []eval.GoldenQuery {
	type candidate struct {
		files    map[string]struct{}
		sessions int
	}
	// key = query text (exact match)
	byQuery := map[string]*candidate{}
	queryOrder := []string{} // insertion order for stable output

	for _, s := range feedback.SplitSessions(events) {
		for i, e := range s {
			if !feedback.IsAskTool(e.ToolName) || e.Query == "" {
				continue
			}
			suggested := make(map[string]struct{}, len(e.Paths))
			for _, p := range e.Paths {
				suggested[filepath.Clean(p)] = struct{}{}
			}

			// Collect consume-tool opens after this ask (within window).
			var missed []string
			consumed := 0
			for j := i + 1; j < len(s); j++ {
				if !feedback.IsConsumeTool(s[j].ToolName) {
					continue
				}
				for _, op := range s[j].Paths {
					rel := relPath(root, op)
					if _, inSuggested := suggested[filepath.Clean(rel)]; !inSuggested {
						if _, inSuggested2 := suggested[filepath.Clean(op)]; !inSuggested2 {
							missed = append(missed, rel)
						}
					}
				}
				consumed++
				if window > 0 && consumed >= window {
					break
				}
			}
			if len(missed) == 0 {
				continue
			}

			c, exists := byQuery[e.Query]
			if !exists {
				c = &candidate{files: map[string]struct{}{}}
				byQuery[e.Query] = c
				queryOrder = append(queryOrder, e.Query)
			}
			c.sessions++
			for _, f := range missed {
				c.files[f] = struct{}{}
			}
		}
	}

	// Sort queries by session_count desc, then alphabetically for stability.
	sort.Slice(queryOrder, func(i, j int) bool {
		ci, cj := byQuery[queryOrder[i]], byQuery[queryOrder[j]]
		if ci.sessions != cj.sessions {
			return ci.sessions > cj.sessions
		}
		return queryOrder[i] < queryOrder[j]
	})

	out := make([]eval.GoldenQuery, 0, len(queryOrder))
	for _, q := range queryOrder {
		c := byQuery[q]
		files := make([]string, 0, len(c.files))
		for f := range c.files {
			files = append(files, f)
		}
		sort.Strings(files)
		out = append(out, eval.GoldenQuery{
			ID:            queryID(q),
			Query:         q,
			RelevantFiles: files,
		})
	}
	return out
}

// relPath makes path relative to root. If path is already relative or
// Rel fails, it returns path unchanged.
func relPath(root, path string) string {
	if !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// queryID returns an 8-char hex prefix of the SHA-256 of the query string,
// used as the GoldenQuery.ID for mined candidates.
func queryID(q string) string {
	h := sha256.Sum256([]byte(q))
	return fmt.Sprintf("m%x", h[:3]) // "m" prefix marks mined; 6 hex chars = 3 bytes
}

func printFeedbackReport(w io.Writer, r feedbackReport) {
	fmt.Fprintf(w, "observe log: %s\n", r.LogPath)
	fmt.Fprintf(w, "events: %d   sessions: %d   asks: %d\n\n", r.Events, r.Sessions, r.Asks)
	fmt.Fprintf(w, "suggested-read open-rate: %s  (%d/%d opened)\n",
		pct(r.OpenRate), r.OpenedReads, r.SuggestedReads)
	fmt.Fprintf(w, "inline-reopen rate:       %s  (%d/%d inlined asks reopened a suggested file)\n",
		pct(r.InlineReopenRate), r.InlineReopened, r.InlinedAsks)

	if len(r.ByIntent) > 0 {
		fmt.Fprintln(w, "\nopen-rate by intent:")
		keys := make([]string, 0, len(r.ByIntent))
		for k := range r.ByIntent {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			st := r.ByIntent[k]
			name := k
			if name == "" {
				name = "(unrouted)"
			}
			fmt.Fprintf(w, "  %-18s %s  (%d/%d over %d asks)\n",
				name, pct(st.OpenRate), st.Opened, st.Suggested, st.Asks)
		}
	}

	if r.SuggestedReads == 0 {
		fmt.Fprintln(w, "\nno ask suggested_reads captured yet — the enriched observe hook records them going forward.")
	}
	fmt.Fprintln(w, "\nCCR pruned-then-re-read rate is measured proxy-side; see `dex proxy --stats`.")
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// runFeedbackShadow joins the #731 shadow log against the observe log and
// reports whether the shadow (lane-agreement reweighted) top-k caught more of
// the files the agent actually opened than the served (static) top-k. This is
// the instrument the #731 data gate turns on: the default reweight stays off
// until this prints a sustained "win-candidate".
func runFeedbackShadow(w io.Writer, events []feedbackEvent, shadowLog string, window int, asJSON bool) error {
	path := shadowLog
	if path == "" {
		path = feedback.DefaultShadowLogPath()
	}
	records, err := feedback.ReadShadowLog(path)
	if err != nil {
		return err
	}
	rep := feedback.AnalyzeShadow(events, records, window)
	rep.ShadowLogPath = path

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printShadowReport(w, rep)
	return nil
}

func printShadowReport(w io.Writer, r feedback.ShadowReport) {
	fmt.Fprintf(w, "shadow log: %s\n", r.ShadowLogPath)
	fmt.Fprintf(w, "records: %d   matched to an ask: %d   divergent top-k: %d\n\n",
		r.Records, r.Matched, r.Reordered)
	fmt.Fprintf(w, "served open-rate: %s  (%d/%d top-k slots opened)\n",
		pct(r.ServedOpenRate), r.ServedOpened, r.ServedSlots)
	fmt.Fprintf(w, "shadow open-rate: %s  (%d/%d top-k slots opened)\n",
		pct(r.ShadowOpenRate), r.ShadowOpened, r.ShadowSlots)
	if r.Reordered > 0 {
		fmt.Fprintf(w, "\non divergent asks: shadow wins %d  /  losses %d\n", r.ShadowWins, r.ShadowLosses)
	}
	fmt.Fprintf(w, "\nverdict: %s\n%s\n", strings.ToUpper(r.Verdict), r.Note)
}
