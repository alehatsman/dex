package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/gitenv"
	"github.com/alehatsman/dex/internal/gitlog"
)

// GoldenQuery is one labeled retrieval example: a query and the set of source
// files used as binary relevance ground truth. Two flavors share this shape:
//   - git-history (golden.go): Query = a commit subject, RelevantFiles = the
//     files that commit touched, Anchor empty.
//   - blast-radius (blastradius.go): Query = a code excerpt from an anchor
//     file, RelevantFiles = the OTHER files co-changed in the same commit,
//     Anchor = the anchor's path (excluded from scoring — it's the given).
type GoldenQuery struct {
	ID            string   `json:"id"`               // short commit hash (+anchor for blast-radius)
	Query         string   `json:"query"`            // query text (commit subject, or anchor code excerpt)
	RelevantFiles []string `json:"relevant_files"`   // repo-relative paths, sorted
	Anchor        string   `json:"anchor,omitempty"` // blast-radius: anchor file path, excluded from ranked results
	Class         string   `json:"class,omitempty"`  // dependency-discoverability class G1/G2/G3 (#549); empty = auto-classify in nav-bench
}

// GoldenSet is a reproducible, committable collection of labeled queries
// mined from a repo's git history at a fixed HEAD.
type GoldenSet struct {
	Repo    string        `json:"repo"`     // repo basename, informational
	Head    string        `json:"head"`     // HEAD hash the set was generated from
	GenOpts GenOpts       `json:"gen_opts"` // generation parameters, for reproducibility
	Queries []GoldenQuery `json:"queries"`  //
}

// GenOpts parameterizes golden-set generation. The same options against the
// same git history produce the same set (modulo HEAD advancing).
type GenOpts struct {
	MaxCommits int `json:"max_commits"` // how many commits to scan (0 → 500)
	MaxFiles   int `json:"max_files"`   // skip commits touching more than this many code files (0 → 5)
}

const (
	genDefaultMaxCommits = 500
	genDefaultMaxFiles   = 5
	genMinQueryLen       = 12 // chars; below this the subject is too terse to be a useful query
)

// codeExts is the set of source-file extensions a touched file must have to
// count as relevant. Keeping relevance to code files avoids labeling docs /
// config churn as retrieval targets.
var codeExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".java": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".rb": true, ".sh": true,
}

// convPrefix matches a leading conventional-commit prefix like
// "feat(mcp): ", "fix: ", "refactor(store)!: " so it can be stripped to
// leave a cleaner natural-language intent (and to avoid the scope token
// acting as an unfair lexical anchor for BM25).
var convPrefix = regexp.MustCompile(`^[a-z]+(\([^)]*\))?!?:\s*`)

// Generate mines git history under root and builds a golden set. It selects
// non-merge commits that touch between 1 and opts.MaxFiles code files (still
// present on disk), using the cleaned commit subject as the query and the
// touched code files as relevant ground truth. The result is deterministic
// for a fixed HEAD and opts.
func Generate(ctx context.Context, root string, opts GenOpts) (GoldenSet, error) {
	if opts.MaxCommits <= 0 {
		opts.MaxCommits = genDefaultMaxCommits
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = genDefaultMaxFiles
	}

	head, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return GoldenSet{}, fmt.Errorf("eval: read HEAD: %w", err)
	}

	commits, err := gitlog.Collect(ctx, root, opts.MaxCommits)
	if err != nil {
		return GoldenSet{}, fmt.Errorf("eval: collect commits: %w", err)
	}

	var queries []GoldenQuery
	seen := make(map[string]bool) // dedup identical query texts
	for _, c := range commits {
		q := cleanSubject(c.Subject)
		if len(q) < genMinQueryLen || seen[q] {
			continue
		}
		var rel []string
		for _, f := range c.Files {
			if !codeExts[strings.ToLower(filepath.Ext(f))] {
				continue
			}
			// Only keep files that still exist on disk — a relevant file
			// that was later deleted can never be retrieved from the index.
			if _, statErr := os.Stat(filepath.Join(root, f)); statErr != nil {
				continue
			}
			rel = append(rel, f)
		}
		if len(rel) == 0 || len(rel) > opts.MaxFiles {
			continue
		}
		sort.Strings(rel)
		seen[q] = true
		queries = append(queries, GoldenQuery{
			ID:            c.ShortHash,
			Query:         q,
			RelevantFiles: rel,
		})
	}

	return GoldenSet{
		Repo:    filepath.Base(root),
		Head:    head,
		GenOpts: opts,
		Queries: queries,
	}, nil
}

// cleanSubject strips a conventional-commit prefix and trims whitespace,
// leaving a natural-language intent suitable as a search query.
func cleanSubject(s string) string {
	return strings.TrimSpace(convPrefix.ReplaceAllString(strings.TrimSpace(s), ""))
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	cmd.Env = gitenv.Current() // prevent hook-injected GIT_DIR from redirecting to wrong repo (#716)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ValidateGolden checks gs for structural issues that silently distort metric
// means: queries with empty RelevantFiles score 0 and inflate the denominator;
// duplicate IDs break QuerySetSHA256; empty IDs/texts are unscorable. Affected
// queries are dropped and described in the returned issue list. For blast-radius
// queries, an anchor present in RelevantFiles is removed (it is excluded from
// ranked results and can never be "hit").
func ValidateGolden(gs GoldenSet) (GoldenSet, []string) {
	var valid []GoldenQuery
	var issues []string
	seen := make(map[string]bool)
	for i, q := range gs.Queries {
		switch {
		case q.ID == "":
			issues = append(issues, fmt.Sprintf("query %d: empty ID, skipping", i))
			continue
		case seen[q.ID]:
			issues = append(issues, fmt.Sprintf("query %q: duplicate ID, skipping duplicate", q.ID))
			continue
		case q.Query == "":
			issues = append(issues, fmt.Sprintf("query %q: empty query text, skipping", q.ID))
			continue
		case len(q.RelevantFiles) == 0:
			issues = append(issues, fmt.Sprintf("query %q: no relevant_files, skipping", q.ID))
			continue
		}
		seen[q.ID] = true
		if q.Anchor != "" {
			var clean []string
			for _, f := range q.RelevantFiles {
				if f != q.Anchor {
					clean = append(clean, f)
				}
			}
			if len(clean) < len(q.RelevantFiles) {
				issues = append(issues, fmt.Sprintf("query %q: anchor %q removed from relevant_files (unreachable in ranked results)", q.ID, q.Anchor))
				q.RelevantFiles = clean
			}
			if len(q.RelevantFiles) == 0 {
				issues = append(issues, fmt.Sprintf("query %q: no relevant_files remain after anchor removal, skipping", q.ID))
				continue
			}
		}
		valid = append(valid, q)
	}
	out := gs
	out.Queries = valid
	return out, issues
}

// LoadGolden reads a golden set from a JSON file.
func LoadGolden(path string) (GoldenSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return GoldenSet{}, err
	}
	var gs GoldenSet
	if err := json.Unmarshal(data, &gs); err != nil {
		return GoldenSet{}, fmt.Errorf("parse golden set %q: %w", path, err)
	}
	return gs, nil
}

// Save writes the golden set as indented JSON.
func (gs GoldenSet) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
