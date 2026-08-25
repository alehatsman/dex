package mcp

import (
	"os"
	"path/filepath"
)

// writeCurrentTask stashes the agent's current task/question at
// ~/.cache/dex/current_task. It is the task source for the #610 adaptive-
// compression feedback loop: the proxy's response hook reads it back
// (readCurrentTask, cmd/dex/proxy.go) to derive intent and record token-ratio
// penalties that self-tune the compression policy.
//
// Before the two-verb collapse (#195) the writer was session(action=set_task).
// With the session tool removed, the read verb feeds the loop instead: query's
// prose/task input is the best available signal for what the agent is doing.
// Best-effort — any error leaves the loop dark, which the reader tolerates.
func writeCurrentTask(task string) {
	if task == "" {
		return
	}
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		var err error
		cacheDir, err = os.UserCacheDir()
		if err != nil {
			return
		}
	}
	p := filepath.Join(cacheDir, "dex", "current_task")
	if mkErr := os.MkdirAll(filepath.Dir(p), 0o755); mkErr != nil {
		return
	}
	_ = os.WriteFile(p, []byte(task), 0o644)
}
