package corpus

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/eval"
	"github.com/alehatsman/dex/internal/store"
)

// Set labels distinguish the golden-set flavors scored for a repo. Curated sets
// keep their file basename so multiple curated sets per repo stay distinct.
const (
	SetGitHistory  = "git-history"
	SetBlastRadius = "blast-radius"
)

// LabeledReport is one (repo, query-set) cell of the corpus report.
type LabeledReport struct {
	Repo   string      `json:"repo"`
	Lang   string      `json:"lang"`
	Set    string      `json:"set"`
	Report eval.Report `json:"report"`
}

// RunRepo scores every declared golden set for one already-indexed repo against
// the live Search path and returns one LabeledReport per set.
//
// It reuses internal/eval wholesale: curated query sets are loaded with
// eval.LoadGolden; enabled gen flavors are generated from the checkout dir with
// eval.Generate / eval.GenerateBlastRadius; each set is scored with eval.Run +
// eval.Compute. st must be an open store for this repo's index; em must use the
// index-recorded embed model (see the CLI wiring). QuerySets are expected to be
// absolute paths (the CLI resolves them against the manifest dir).
func RunRepo(ctx context.Context, em embed.Embedder, st *store.Store, spec RepoSpec, dir string, k int) ([]LabeledReport, error) {
	lang := ""
	if len(spec.Languages) > 0 {
		lang = spec.Languages[0]
	}

	var out []LabeledReport
	score := func(setLabel string, gs eval.GoldenSet, keepSummaries bool) error {
		if len(gs.Queries) == 0 {
			return nil // nothing to score (e.g. a repo with no qualifying history)
		}
		results, err := eval.Run(ctx, em, st, gs, k, keepSummaries)
		if err != nil {
			return fmt.Errorf("corpus: score %s/%s: %w", spec.Name, setLabel, err)
		}
		out = append(out, LabeledReport{
			Repo:   spec.Name,
			Lang:   lang,
			Set:    setLabel,
			Report: eval.Compute(results, k),
		})
		return nil
	}

	// Curated query sets (hand-authored NL queries → relevant files).
	for _, qs := range spec.QuerySets {
		gs, err := eval.LoadGolden(qs)
		if err != nil {
			return nil, fmt.Errorf("corpus: load query set %q: %w", qs, err)
		}
		if err := score("curated:"+filepath.Base(qs), gs, false); err != nil {
			return nil, err
		}
	}

	// Auto-generated golden sets mined from the repo's own history at its pin.
	if spec.Gen.GitHistory.Enabled {
		gs, err := eval.Generate(ctx, dir, genOpts(spec.Gen.GitHistory))
		if err != nil {
			return nil, fmt.Errorf("corpus: generate git-history for %s: %w", spec.Name, err)
		}
		if err := score(SetGitHistory, gs, false); err != nil {
			return nil, err
		}
	}
	if spec.Gen.BlastRadius.Enabled {
		gs, err := eval.GenerateBlastRadius(ctx, dir, genOpts(spec.Gen.BlastRadius))
		if err != nil {
			return nil, fmt.Errorf("corpus: generate blast-radius for %s: %w", spec.Name, err)
		}
		if err := score(SetBlastRadius, gs, false); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func genOpts(g GenSpec) eval.GenOpts {
	return eval.GenOpts{MaxCommits: g.MaxCommits, MaxFiles: g.MaxFiles}
}
