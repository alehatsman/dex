package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// entropyFiltered drops low-information lines from text (entropy mode), keeping
// the original lines when the filter would strip everything.
func entropyFiltered(text string) string {
	ranged := strings.Split(text, "\n")
	filtered := compress.EntropyFilter(ranged, compress.EntropyThresholdStandard)
	if filtered == nil {
		return strings.Join(ranged, "\n")
	}
	return strings.Join(filtered, "\n")
}

// maybeCompactJSON losslessly compacts whole-file JSON/JSONL content, stripping
// insignificant whitespace with zero semantic loss (#619). Returns the input
// unchanged for non-JSON extensions or unparseable content.
func maybeCompactJSON(content, ext string) string {
	switch strings.ToLower(ext) {
	case ".jsonl", ".ndjson":
		if c, ok := compress.CompactJSONL(content); ok {
			return c
		}
	case ".json":
		if c, ok := compress.CompactJSON(content); ok {
			return c
		}
	}
	return content
}

// readSource returns the file content from the working tree, or from a git ref
// when ref is non-empty (#656).
func readSource(ctx context.Context, path, ref string) ([]byte, error) {
	if ref != "" {
		return gitShowFile(ctx, path, ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// gitShowFile returns the contents of path as of git ref (#656 / #644 slice).
// It locates the file's git repository, makes the path repo-relative, and runs
// `git show <ref>:<relpath>` so the read works from any subdirectory. Used by
// `dex read --ref` to time-travel a file's content.
func gitShowFile(ctx context.Context, path, ref string) ([]byte, error) {
	if strings.HasPrefix(ref, "-") {
		// A ref starting with '-' would be parsed by git as an option, not a
		// revision — reject rather than risk option injection.
		return nil, fmt.Errorf("invalid --ref %q (must not start with '-')", ref)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)
	rootOut, err := gitRead(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository at %s: %w", dir, err)
	}
	root := strings.TrimSpace(string(rootOut))
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return nil, err
	}
	out, err := gitRead(ctx, root, "show", ref+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s — check the ref and that the file existed then: %w", ref, rel, err)
	}
	return out, nil
}

// gitRead runs a read-only git command in dir with a hermetic environment.
// GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE are scrubbed so an injected value
// (e.g. from a hook) can't redirect git to the wrong repository (#341).
func gitRead(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hermeticGitReadEnv()
	return cmd.Output()
}

func hermeticGitReadEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="),
			strings.HasPrefix(kv, "GIT_WORK_TREE="),
			strings.HasPrefix(kv, "GIT_INDEX_FILE="):
			continue
		}
		out = append(out, kv)
	}
	return out
}
