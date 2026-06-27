package feedback

import (
	"os"
	"path/filepath"
)

// DefaultLogDir returns the directory holding the observe log, honoring
// XDG_DATA_HOME (falling back to ~/.local/share). Empty string if the home
// dir can't be resolved. This is the single resolver shared by the hook
// writer (cmd/dex) and the live reader (internal/mcp).
func DefaultLogDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "dex")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "dex")
}

// DefaultLogPath is the observe log (hooks.jsonl) the PostToolUse hook writes.
func DefaultLogPath() string {
	dir := DefaultLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "hooks.jsonl")
}

// DefaultShadowLogPath is where the live reweighter records its A/B shadow
// rankings (#731). It sits beside the observe log so the same gauge tooling
// can find it.
func DefaultShadowLogPath() string {
	dir := DefaultLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "feedback_shadow.jsonl")
}
