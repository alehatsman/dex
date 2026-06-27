package main

import (
	"bufio"
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
)

// `dex feedback` reads the observe log (hooks.jsonl) the PostToolUse hook
// writes and turns it into a relevance signal on real traffic (#724). The
// log was write-only until this consumer existed: nothing measured whether
// the reads `ask` recommended were the reads the agent actually opened.
//
// It computes, per session (segmented by the Stop / PreCompact boundary
// events the hook records):
//
//   - suggested-read open-rate: of the reads `ask` recommended, what
//     fraction the agent opened afterward — the core "is ask pointing where
//     the agent goes" number, broken down by routed intent;
//   - inline-reopen rate: of `ask` calls that inlined content, what fraction
//     were followed by re-opening a recommended file anyway (inline bytes
//     spent without saving the read).
//
// The pruned-then-re-read (CCR) rate is already measured proxy-side
// (rereads_after_stub in the proxy request log); see `dex proxy --stats`.

type feedbackEvent struct {
	TS       int64    `json:"ts"`
	Event    string   `json:"event,omitempty"`
	ToolName string   `json:"tool_name,omitempty"`
	Tokens   int      `json:"tokens,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Inlined  int      `json:"inlined_bytes,omitempty"`
	Intent   string   `json:"intent,omitempty"`
	Query    string   `json:"query,omitempty"` // ask-only: question text for miss-mining (#732)
}

type intentStat struct {
	Asks      int     `json:"asks"`
	Suggested int     `json:"suggested"`
	Opened    int     `json:"opened"`
	OpenRate  float64 `json:"open_rate"`
}

type feedbackReport struct {
	LogPath          string                `json:"log_path"`
	Events           int                   `json:"events"`
	Sessions         int                   `json:"sessions"`
	Asks             int                   `json:"asks"`
	SuggestedReads   int                   `json:"suggested_reads"`
	OpenedReads      int                   `json:"opened_reads"`
	OpenRate         float64               `json:"open_rate"`
	InlinedAsks      int                   `json:"inlined_asks"`
	InlineReopened   int                   `json:"inline_reopened"`
	InlineReopenRate float64               `json:"inline_reopen_rate"`
	ByIntent         map[string]intentStat `json:"by_intent,omitempty"`
}

func runFeedback(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("feedback", flag.ContinueOnError)
	logPath := fs.String("log", "", "path to hooks.jsonl (default: $XDG_DATA_HOME/dex/hooks.jsonl)")
	window := fs.Int("window", 0, "max consume events to look ahead per suggested read within a session (0 = whole session)")
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	mineCurated := fs.Bool("mine-curated", false, "emit GoldenQuery candidates mined from missed asks (JSON array, review before adding to curated.json)")
	projectPath := fs.String("project", ".", "project root — used to make absolute opened paths relative (for --mine-curated)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *logPath
	if path == "" {
		path = filepath.Join(hookLogDir(), "hooks.jsonl")
	}
	events, err := readFeedbackLog(path)
	if err != nil {
		return err
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

	rep := computeFeedback(events, *window)
	rep.LogPath = path

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printFeedbackReport(os.Stdout, rep)
	return nil
}

// readFeedbackLog parses hooks.jsonl into events. Malformed lines are skipped
// (the log is appended live and a partial final line is normal).
func readFeedbackLog(path string) ([]feedbackEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open observe log: %w (run some sessions first, or pass --log)", err)
	}
	defer func() { _ = f.Close() }()

	var out []feedbackEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e feedbackEvent
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// computeFeedback joins ask recommendations against subsequent reads. The
// log is split into sessions on boundary events (Stop / PreCompact); the
// join never crosses a boundary, so a read in a later session can't be
// credited to an earlier session's ask. window bounds the lookahead in
// consume events per suggested read (0 = rest of session).
func computeFeedback(events []feedbackEvent, window int) feedbackReport {
	rep := feedbackReport{Events: len(events), ByIntent: map[string]intentStat{}}

	var sessions [][]feedbackEvent
	var cur []feedbackEvent
	for _, e := range events {
		if e.Event != "" { // boundary event ends the current session
			if len(cur) > 0 {
				sessions = append(sessions, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, e)
	}
	if len(cur) > 0 {
		sessions = append(sessions, cur)
	}
	rep.Sessions = len(sessions)

	for _, s := range sessions {
		for i, e := range s {
			if !isAskTool(e.ToolName) || len(e.Paths) == 0 {
				continue
			}
			rep.Asks++
			st := rep.ByIntent[e.Intent]
			st.Asks++

			openedAny := false
			for _, sp := range e.Paths {
				rep.SuggestedReads++
				st.Suggested++
				if sessionOpens(s, i+1, sp, window) {
					rep.OpenedReads++
					st.Opened++
					openedAny = true
				}
			}
			rep.ByIntent[e.Intent] = st

			if e.Inlined > 0 {
				rep.InlinedAsks++
				if openedAny {
					rep.InlineReopened++
				}
			}
		}
	}

	if rep.SuggestedReads > 0 {
		rep.OpenRate = ratio(rep.OpenedReads, rep.SuggestedReads)
	}
	if rep.InlinedAsks > 0 {
		rep.InlineReopenRate = ratio(rep.InlineReopened, rep.InlinedAsks)
	}
	for k, st := range rep.ByIntent {
		if st.Suggested > 0 {
			st.OpenRate = ratio(st.Opened, st.Suggested)
		}
		rep.ByIntent[k] = st
	}
	return rep
}

// sessionOpens reports whether suggested path sp is opened by a consume tool
// at an index after from, within window consume events (window <= 0 = to end
// of session).
func sessionOpens(s []feedbackEvent, from int, sp string, window int) bool {
	consumed := 0
	for j := from; j < len(s); j++ {
		if !isConsumeTool(s[j].ToolName) {
			continue
		}
		for _, op := range s[j].Paths {
			if pathMatch(sp, op) {
				return true
			}
		}
		consumed++
		if window > 0 && consumed >= window {
			break
		}
	}
	return false
}

// pathMatch reports whether a suggested path and an opened path refer to the
// same file. ask emits repo-relative paths; Read often passes an absolute
// path — so a component-boundary suffix match either direction counts.
func pathMatch(suggested, opened string) bool {
	a := filepath.Clean(suggested)
	b := filepath.Clean(opened)
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasSuffix(b, sep+a) || strings.HasSuffix(a, sep+b)
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
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

	// Split into sessions, same logic as computeFeedback.
	var sessions [][]feedbackEvent
	var cur []feedbackEvent
	for _, e := range events {
		if e.Event != "" {
			if len(cur) > 0 {
				sessions = append(sessions, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, e)
	}
	if len(cur) > 0 {
		sessions = append(sessions, cur)
	}

	for _, s := range sessions {
		for i, e := range s {
			if !isAskTool(e.ToolName) || e.Query == "" {
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
				if !isConsumeTool(s[j].ToolName) {
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
