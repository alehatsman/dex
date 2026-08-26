package proj

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alehatsman/dex/internal/store"
)

// KnownRoots walks base (the index cache dir, one subdirectory per indexed
// project keyed by Project.ID) and reads each project's recorded
// `project_root` meta. Entries written before that meta existed are skipped
// with a warning to warn — the caller can `dex nuke <path>` + `dex index
// <path>` once to re-record it.
//
// Shared by `dex reindex --all` (cmd/dex) and the query fan-out lane's
// project_roots: ["all"] sentinel (internal/mcp), so both discover the same
// set of locally indexed projects the same way.
func KnownRoots(ctx context.Context, base string, warn func(format string, args ...any)) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if warn == nil {
		warn = func(string, ...any) {}
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dbPath := filepath.Join(base, e.Name(), "index.db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		st, err := store.Open(ctx, dbPath)
		if err != nil {
			warn("skipping %s: open: %v", e.Name(), err)
			continue
		}
		root, err := st.ProjectRoot(ctx)
		_ = st.Close()
		if err != nil {
			warn("skipping %s: %v", e.Name(), err)
			continue
		}
		if root == "" {
			warn("skipping %s: no recorded project_root (pre-migration index)", e.Name())
			continue
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// WarnStderr is a ready-made KnownRoots warn func for CLI callers.
func WarnStderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}
