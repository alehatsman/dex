package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alehatsman/dex/internal/eval"
)

// cachedGoldenSet memoizes a generated golden set to disk, keyed by repo,
// commit pin, set type, and gen opts.
//
// The git-mined sets (git-history / blast-radius / structural) are
// deterministic for a fixed checkout, so a calibration sweep that re-scores the
// same repos under many fusion settings would regenerate identical sets on
// every run — pure CPU/git waste that leaves the GPU idle. Caching turns the
// second run onward into a JSON read. The cache key embeds the commit SHA and
// gen opts (in the filename), so moving a repo's pin or changing
// MaxCommits/MaxFiles yields a fresh entry rather than a stale hit. A read that
// fails for any reason (missing, corrupt, unreadable) falls through to a fresh
// generate, so the cache can never wedge a run.
//
// cacheDir == "" disables caching (callers without a cache root).
func cachedGoldenSet(cacheDir, repo, commit, setType string, o eval.GenOpts, gen func() (eval.GoldenSet, error)) (eval.GoldenSet, error) {
	if cacheDir == "" {
		return gen()
	}
	path := filepath.Join(cacheDir,
		fmt.Sprintf("%s@%s-%s-c%d-f%d.json", repo, shortSHA(commit), setType, o.MaxCommits, o.MaxFiles))

	if b, err := os.ReadFile(path); err == nil {
		var gs eval.GoldenSet
		if json.Unmarshal(b, &gs) == nil {
			return gs, nil
		}
	}

	gs, err := gen()
	if err != nil {
		return gs, err
	}
	// Best-effort write: a failure just means the next run regenerates.
	if mkErr := os.MkdirAll(cacheDir, 0o755); mkErr == nil {
		if b, mErr := json.MarshalIndent(gs, "", "  "); mErr == nil {
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return gs, nil
}
