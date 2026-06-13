package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// hookObserve handles PostToolUse, Stop, and PreCompact. It appends a compact
// event record to $XDG_DATA_HOME/dex/hooks.jsonl. No stdout output.
func hookObserve() error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		return nil
	}

	var v map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}

	type event struct {
		TS       int64  `json:"ts"`
		ToolName string `json:"tool_name,omitempty"`
		Tokens   int    `json:"tokens,omitempty"`
	}
	ev := event{TS: time.Now().Unix()}

	if raw, ok := v["tool_name"]; ok {
		_ = json.Unmarshal(raw, &ev.ToolName)
	}
	if raw, ok := v["tool_input"]; ok {
		ev.Tokens = len(raw) / 4 // rough 4-bytes-per-token estimate
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
	return nil
}

func hookLogDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "dex")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "dex")
}
