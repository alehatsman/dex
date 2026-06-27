// Package feedback reads the observe log (hooks.jsonl) the PostToolUse hook
// writes (#724) and turns it into a relevance signal over real traffic: of the
// reads `ask` recommended, which did the agent actually open, broken down by
// routed intent.
//
// It is the single in-process home for the hooks.jsonl reader and the
// ask-vs-read join. The `dex feedback` CLI consumes it as an offline gauge;
// internal/mcp consumes it live as the substrate for online lane reweighting
// (#731). Keeping one parser here prevents a second, drifting copy — the exact
// class of bug #734 was (a payload-shape parser that the CLI test never
// exercised against the real envelope).
package feedback

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Event mirrors one line of hooks.jsonl. A boundary event (Stop / PreCompact)
// carries Event != "" and no tool fields; a tool event carries ToolName.
type Event struct {
	TS       int64    `json:"ts"`
	Event    string   `json:"event,omitempty"`
	ToolName string   `json:"tool_name,omitempty"`
	Tokens   int      `json:"tokens,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Inlined  int      `json:"inlined_bytes,omitempty"`
	Intent   string   `json:"intent,omitempty"`
	Query    string   `json:"query,omitempty"` // ask-only: question text for miss-mining (#732)
}

// IntentStat is the per-intent open-rate breakdown.
type IntentStat struct {
	Asks      int     `json:"asks"`
	Suggested int     `json:"suggested"`
	Opened    int     `json:"opened"`
	OpenRate  float64 `json:"open_rate"`
}

// Report is the joined relevance signal over the whole log.
type Report struct {
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
	ByIntent         map[string]IntentStat `json:"by_intent,omitempty"`
}

// IsAskTool reports whether name is a dex `ask` call (the MCP tool is
// mcp__dex__ask; the bare name covers a future rename or a direct caller).
func IsAskTool(name string) bool {
	return name == "ask" || strings.HasSuffix(name, "__ask")
}

// IsConsumeTool reports whether name is a tool that opens a file by path —
// the consumption side of the suggested-read join.
func IsConsumeTool(name string) bool {
	switch name {
	case "Read", "Edit", "MultiEdit", "Write", "NotebookEdit":
		return true
	}
	return strings.HasSuffix(name, "__read") // mcp__dex__read
}

// ReadLog parses hooks.jsonl into events. Malformed lines are skipped (the log
// is appended live and a partial final line is normal).
func ReadLog(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open observe log: %w (run some sessions first, or pass --log)", err)
	}
	defer func() { _ = f.Close() }()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Compute joins ask recommendations against subsequent reads. The log is split
// into sessions on boundary events (Stop / PreCompact); the join never crosses
// a boundary, so a read in a later session can't be credited to an earlier
// session's ask. window bounds the lookahead in consume events per suggested
// read (0 = rest of session).
func Compute(events []Event, window int) Report {
	rep := Report{Events: len(events), ByIntent: map[string]IntentStat{}}

	sessions := splitSessions(events)
	rep.Sessions = len(sessions)

	for _, s := range sessions {
		for i, e := range s {
			if !IsAskTool(e.ToolName) || len(e.Paths) == 0 {
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

// SplitSessions segments a flat event stream into per-session slices on
// boundary events (Event != ""). Exported because the miss-miner (#732) needs
// the same segmentation as the join.
func SplitSessions(events []Event) [][]Event { return splitSessions(events) }

func splitSessions(events []Event) [][]Event {
	var sessions [][]Event
	var cur []Event
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
	return sessions
}

// sessionOpens reports whether suggested path sp is opened by a consume tool at
// an index after from, within window consume events (window <= 0 = to end of
// session).
func sessionOpens(s []Event, from int, sp string, window int) bool {
	consumed := 0
	for j := from; j < len(s); j++ {
		if !IsConsumeTool(s[j].ToolName) {
			continue
		}
		for _, op := range s[j].Paths {
			if PathMatch(sp, op) {
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

// PathMatch reports whether a suggested path and an opened path refer to the
// same file. ask emits repo-relative paths; Read often passes an absolute path
// — so a component-boundary suffix match either direction counts.
func PathMatch(suggested, opened string) bool {
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
