package index

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const gitSubjectsTimeout = 300 * time.Millisecond

// RecentCommitSubjects returns up to n commit subjects for relPath inside
// projectRoot. Returns nil on any error: missing git, no history, or timeout.
func RecentCommitSubjects(ctx context.Context, projectRoot, relPath string, n int) []string {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, gitSubjectsTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git",
		"-C", projectRoot,
		"log", fmt.Sprintf("-%d", n),
		"--format=%s",
		"--", relPath,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects
}
