package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/alehatsman/dex/internal/feedback"
)

const toolSearch = "ToolSearch"

// hookObserve handles PostToolUse and Stop. It appends a compact event record
// to $XDG_DATA_HOME/dex/hooks.jsonl. No stdout output.
func hookObserve() error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		return nil
	}

	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}

	var hookEventName string
	if r, ok := v["hook_event_name"]; ok {
		_ = json.Unmarshal(r, &hookEventName)
	}

	// event is one line of hooks.jsonl. paths carries the join keys the
	// feedback consumer needs (#724): for a read/edit tool it is the file
	// consumed (from tool_input); for an ask call it is the suggested_reads
	// the bundle recommended (from tool_response). inlined_bytes, intent, and
	// query are ask-only. event names a session boundary (Stop / PreCompact)
	// so the consumer can window the join; it is omitted for the common
	// PostToolUse. query is recorded for curated-golden miss-mining (#732).
	type event struct {
		TS       int64    `json:"ts"`
		Event    string   `json:"event,omitempty"`
		ToolName string   `json:"tool_name,omitempty"`
		Tokens   int      `json:"tokens,omitempty"`
		Paths    []string `json:"paths,omitempty"`
		Inlined  int      `json:"inlined_bytes,omitempty"`
		Intent   string   `json:"intent,omitempty"`
		Query    string   `json:"query,omitempty"`
	}
	ev := event{TS: time.Now().Unix()}
	if hookEventName != "" && hookEventName != "PostToolUse" {
		ev.Event = hookEventName
	}

	if raw, ok := v["tool_name"]; ok {
		_ = json.Unmarshal(raw, &ev.ToolName)
	}
	if raw, ok := v["tool_input"]; ok {
		ev.Tokens = len(raw) / 4 // rough 4-bytes-per-token estimate
	}
	switch {
	case feedback.IsAskTool(ev.ToolName):
		// Recommendations the bundle made — joined against later reads.
		ev.Paths, ev.Inlined, ev.Intent = parseAskResponse(v["tool_response"])
		ev.Query = parseAskInput(v["tool_input"])
	case feedback.IsConsumeTool(ev.ToolName):
		// A file the agent actually opened — the consumption side of the join.
		ev.Paths = pathsFromInput(v["tool_input"])
	}

	logDir := hookLogDir()
	if logDir == "" {
		return nil
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(logDir, "hooks.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))

	// When ToolSearch fires, schemas are now loaded — touch the sentinel so
	// schemasNudge in hookInject stops firing for the next 30 minutes.
	if ev.ToolName == toolSearch {
		if sentinel := schemasLoadedSentinelPath(); sentinel != "" {
			if mkErr := os.MkdirAll(filepath.Dir(sentinel), 0o755); mkErr == nil {
				if sf, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
					_ = sf.Close()
				}
			}
		}
	}

	return nil
}

// hookLogDir resolves the observe-log directory via the shared resolver in
// internal/feedback (one home for both the writer here and the live reader).
func hookLogDir() string { return feedback.DefaultLogDir() }
