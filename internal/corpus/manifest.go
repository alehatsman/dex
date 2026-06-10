// Package corpus runs the dex retrieval eval (internal/eval) across a set of
// pinned real-world repositories, instead of only dex's own git history.
//
// A committed manifest (benchmark/corpus/repos.yml) lists repos pinned at fixed
// commits. Each repo carries one or more committed query sets (curated
// eval.GoldenSet JSON) and/or opt-in auto-generated golden sets mined from the
// repo's own history. The runner fetches each repo at its pin into a cache,
// scores the live Search path against every declared set, and aggregates a
// per-(repo, set) report with a per-cell regression gate.
//
// This generalizes the single-repo eval gate (internal/eval, #247) to multiple
// languages so retrieval tuning is validated beyond dex's own Go codebase
// (#278, epic #246). Nothing in internal/eval is modified — corpus reuses
// eval.GoldenSet, eval.Run, eval.Generate/GenerateBlastRadius and eval.Report.
package corpus

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// GenSpec opts a repo into one flavor of auto-generated golden set, mined from
// the fetched repo's own git history at its pinned commit. MaxCommits/MaxFiles
// map 1:1 to eval.GenOpts; zero means the eval-package default.
type GenSpec struct {
	Enabled    bool `yaml:"enabled"`
	MaxCommits int  `yaml:"max_commits"`
	MaxFiles   int  `yaml:"max_files"`
}

// GenConfig groups the optional auto-label flavors for a repo.
type GenConfig struct {
	GitHistory  GenSpec `yaml:"git_history"`
	BlastRadius GenSpec `yaml:"blast_radius"`
}

// RepoSpec is one pinned repository in the corpus.
type RepoSpec struct {
	Name      string   `yaml:"name"`      // cache key + report label; [a-z0-9-], unique
	URL       string   `yaml:"url"`       // clone URL
	Commit    string   `yaml:"commit"`    // 40-hex pinned commit (a release tag's commit)
	Languages []string `yaml:"languages"` // first entry is the primary language for rollups

	// IndexSubdir optionally narrows indexing (and therefore relevant-file
	// paths) to a subtree — used to bound the index cost of large monorepos.
	// When set, curated query-set paths must be relative to this subtree.
	IndexSubdir string `yaml:"index_subdir"`

	// QuerySets are dex-repo-relative paths to committed eval.GoldenSet JSON
	// files (curated NL queries). The CLI resolves them to absolute paths
	// before scoring.
	QuerySets []string `yaml:"query_sets"`

	// Gen opts the repo into auto-generated golden sets in addition to (or
	// instead of) curated query sets.
	Gen GenConfig `yaml:"gen"`
}

// Manifest is the parsed benchmark/corpus/repos.yml.
type Manifest struct {
	Repos []RepoSpec `yaml:"repos"`
}

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// LoadManifest reads and parses a corpus manifest YAML file. It does not
// validate semantics — call Validate for that.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse corpus manifest %q: %w", path, err)
	}
	return m, nil
}

// Validate checks the manifest is internally consistent: non-empty, unique
// lowercase names, an http(s) URL, a 40-hex commit, and at least one source of
// queries (a curated set or an enabled gen flavor) per repo.
func (m Manifest) Validate() error {
	if len(m.Repos) == 0 {
		return fmt.Errorf("corpus manifest: no repos")
	}
	seen := make(map[string]bool, len(m.Repos))
	for i, r := range m.Repos {
		where := fmt.Sprintf("repo[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("repo %q", r.Name)
		}
		if !nameRe.MatchString(r.Name) {
			return fmt.Errorf("%s: name must match %s", where, nameRe)
		}
		if seen[r.Name] {
			return fmt.Errorf("%s: duplicate name", where)
		}
		seen[r.Name] = true
		if !isHTTPURL(r.URL) {
			return fmt.Errorf("%s: url must be http(s)", where)
		}
		if !commitRe.MatchString(r.Commit) {
			return fmt.Errorf("%s: commit must be a 40-hex sha (got %q)", where, r.Commit)
		}
		if len(r.Languages) == 0 {
			return fmt.Errorf("%s: at least one language required", where)
		}
		if len(r.QuerySets) == 0 && !r.Gen.GitHistory.Enabled && !r.Gen.BlastRadius.Enabled {
			return fmt.Errorf("%s: no query source (add a query_set or enable a gen flavor)", where)
		}
	}
	return nil
}

func isHTTPURL(s string) bool {
	return len(s) > 8 && (s[:7] == "http://" || s[:8] == "https://")
}
