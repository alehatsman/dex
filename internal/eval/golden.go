package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

	commits, err := collectCommits(ctx, root, opts.MaxCommits)
	if err != nil {
		return GoldenSet{}, fmt.Errorf("eval: collect commits: %w", err)
	}

	var queries []GoldenQuery
	seen := make(map[string]bool) // dedup identical query texts
	for _, c := range commits {
		q := cleanSubject(c.subject)
		if len(q) < genMinQueryLen || seen[q] {
			continue
		}
		var rel []string
		for _, f := range c.files {
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
			ID:            c.shortHash,
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

type commitRec struct {
	shortHash string
	subject   string
	files     []string
}

// collectCommits runs git log and parses subject + changed-file list per
// non-merge commit. With --name-only the output is a sequence of blocks:
//
//	<hash>\x00<subject>      ← metadata line (the only line containing a NUL)
//	                         ← blank
//	path/one.go              ← changed files, one per line
//	path/two.go
//
// followed by the next commit's block. We detect metadata lines by the NUL
// they carry; every non-empty line in between is a changed file of the
// current commit. This avoids any sentinel-placement fragility.
func collectCommits(ctx context.Context, root string, max int) ([]commitRec, error) {
	args := []string{
		"log",
		"--no-merges",
		"--format=%H%x00%s",
		"--name-only",
		// --relative makes git both (a) restrict output to files under the cwd
		// subtree and (b) emit pathnames relative to it. When root is the repo
		// root this is a no-op (relative to root == repo-root-relative, whole
		// tree); when root is an index_subdir (e.g. packages/react-dom-bindings)
		// it filters to that package AND rebases paths to subdir-relative, so the
		// generated relevant_files match how the index records paths and the
		// downstream os.Stat(root/f) existence check resolves. Without it, a
		// subdir root yields repo-root-relative paths that fail that check,
		// silently dropping every file and producing an empty golden set (#285).
		"--relative",
		fmt.Sprintf("--max-count=%d", max),
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}

	var recs []commitRec
	var cur *commitRec
	for _, raw := range bytes.Split(out, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		if i := bytes.IndexByte(line, 0); i >= 0 {
			// Metadata line: starts a new commit record.
			hash := strings.TrimSpace(string(line[:i]))
			subject := strings.TrimSpace(string(line[i+1:]))
			if len(hash) < 8 || subject == "" {
				cur = nil
				continue
			}
			recs = append(recs, commitRec{shortHash: hash[:8], subject: subject})
			cur = &recs[len(recs)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if f := strings.TrimSpace(string(line)); f != "" {
			cur.files = append(cur.files, f)
		}
	}
	return recs, nil
}

func gitOutput(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
