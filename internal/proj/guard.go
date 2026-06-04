package proj

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const EnvAllowPaths = "DEX_ALLOW_PATHS"

var systemHardDeny = map[string]struct{}{
	"/":             {},
	"/home":         {},
	"/Users":        {},
	"/usr":          {},
	"/var":          {},
	"/etc":          {},
	"/proc":         {},
	"/sys":          {},
	"/tmp":          {},
	"/mnt":          {},
	"/run":          {},
	"/dev":          {},
	"/boot":         {},
	"/opt":          {},
	"/srv":          {},
	"/lost+found":   {},
	"/System":       {},
	"/Library":      {},
	"/Applications": {},
	"/private":      {},
	"/cores":        {},
	"/Volumes":      {},
	"/Network":      {},
}

func CheckIndexable(p *Project, force bool) error {
	if force {
		return nil
	}
	root := filepath.Clean(p.Root)
	if _, hard := systemHardDeny[root]; hard {
		return fmt.Errorf("refusing to index %s: protected system path (re-run with --force if you really mean it)", root)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && root == filepath.Clean(home) {
		return fmt.Errorf("refusing to index %s: that's your home directory (re-run with --force if you really mean it)", root)
	}
	if isInGitWorkTree(root) {
		return nil
	}
	// Resolve symlinks before allowlist comparison only — on macOS /var → /private/var.
	// We keep `root` un-resolved above so hard-deny and home-dir checks use the
	// canonical path the caller passed in (consistent with os.UserHomeDir output).
	resolvedRoot := root
	if real, err := filepath.EvalSymlinks(root); err == nil {
		resolvedRoot = real
	}
	for _, prefix := range allowlistPrefixes() {
		if pathHasPrefix(resolvedRoot, prefix) {
			return nil
		}
	}
	return fmt.Errorf("refusing to index %s: not inside a git work tree and not under any %s prefix (re-run with --force, or add a prefix to %s)", root, EnvAllowPaths, EnvAllowPaths)
}

// .git is a file (not a dir) inside git worktrees and submodules.
func isInGitWorkTree(path string) bool {
	for {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return false
		}
		path = parent
	}
}

func allowlistPrefixes() []string {
	raw := os.Getenv(EnvAllowPaths)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, string(filepath.ListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = expandHome(strings.TrimSpace(p))
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		// On macOS /var → /private/var; resolve so the prefix matches
		// a Project.Root that went through filepath.EvalSymlinks.
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		out = append(out, p)
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return os.ExpandEnv(p)
}

// strings.HasPrefix alone would falsely match "/foo" against "/foobar".
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(path, prefix)
}
