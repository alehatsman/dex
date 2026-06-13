package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/store"
)

// PrefetchInput describes a ctx_prefetch call.
type PrefetchInput struct {
	// ChangedFiles lists files the agent has edited or wants the blast radius of.
	// At least one file is required. Paths may be absolute or relative to project_root.
	ChangedFiles []string `json:"changed_files" jsonschema:"files to compute blast-radius from; at least one required"`
	// Task is an optional free-text description of the current task.
	// When provided, task keywords are used to boost relevance scoring.
	Task string `json:"task,omitempty" jsonschema:"optional current task description for relevance boosting"`
	// MaxFiles caps the number of blast-radius files to prefetch (default 10, max 20).
	MaxFiles int `json:"max_files,omitempty" jsonschema:"max blast-radius files to prefetch (1-20, default 10)"`
	// BudgetTokens is the agent's remaining context budget in tokens.
	// When set, dex selects read fidelity per file using a budget-ratio strategy:
	//   remaining/budget ≥ 0.8 → full (LLM summary), ≥ 0.4 → map, else → signatures.
	// When omitted, all files are read in "signatures" mode.
	BudgetTokens int `json:"budget_tokens,omitempty" jsonschema:"remaining context budget in tokens; drives auto mode selection (full/map/signatures)"`
	// ProjectRoot is the absolute path to the project root (defaults to server working dir).
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// PrefetchFile is one prefetched file in the result.
type PrefetchFile struct {
	Path    string `json:"path"`             // project-relative path
	Mode    string `json:"mode"`             // "full" | "map" | "signatures"
	Tokens  int    `json:"tokens,omitempty"` // estimated token count of returned content
	Content string `json:"content,omitempty"`
}

// PrefetchOutput is the response from ctx_prefetch.
type PrefetchOutput struct {
	Status       string         `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint         string         `json:"hint,omitempty"`
	Project      string         `json:"project,omitempty"`
	Total        int            `json:"total"` // total blast-radius candidates found
	Files        []PrefetchFile `json:"files"`
	Seeds        []string       `json:"seeds,omitempty"`         // resolved seed paths
	SkippedSeeds []string       `json:"skipped_seeds,omitempty"` // seeds not found in index
}

func (s *Server) Prefetch(ctx context.Context, in PrefetchInput) (PrefetchOutput, error) {
	_, out, err := s.prefetch(ctx, nil, in)
	return out, err
}

func (s *Server) prefetch(ctx context.Context, _ *sdk.CallToolRequest, in PrefetchInput) (*sdk.CallToolResult, PrefetchOutput, error) {
	if len(in.ChangedFiles) == 0 {
		return nil, PrefetchOutput{Status: "error", Hint: "changed_files is required"}, nil
	}

	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, PrefetchOutput{Status: "error", Hint: hint}, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, PrefetchOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	maxFiles := in.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 10
	}
	if maxFiles > 20 {
		maxFiles = 20
	}

	// Resolve and normalize seed paths.
	seeds, skipped := resolveSeedPaths(p.Root, in.ChangedFiles)
	if len(seeds) == 0 {
		return nil, PrefetchOutput{
			Status:       "ok",
			Project:      p.Root,
			Hint:         "none of the changed_files are in the project index",
			SkippedSeeds: skipped,
		}, nil
	}

	// Spread activation from seed files to discover blast-radius neighbors.
	seedWeights := make([]store.SeedFile, len(seeds))
	for i, s := range seeds {
		seedWeights[i] = store.SeedFile{Path: s, Weight: 1.0}
	}
	neighbors, err := st.SpreadActivation(ctx, seedWeights, maxFiles*3) // over-fetch; re-score below
	if err != nil {
		return nil, PrefetchOutput{Status: "error", Project: p.Root,
			Hint: fmt.Sprintf("graph traversal: %v", err)}, nil
	}

	total := len(neighbors)
	if total == 0 {
		return nil, PrefetchOutput{
			Status:  "ok",
			Project: p.Root,
			Hint:    "no blast-radius neighbors found; graph may not be indexed or project has no imports",
			Seeds:   seeds,
			Total:   0,
		}, nil
	}

	// Score by session recency and task-keyword overlap.
	// Session files are fetched best-effort; failure is non-fatal (falls back
	// to task-only ordering).
	var sessionFiles map[string]struct{}
	if ss, ok, err := st.SessionGet(ctx); err == nil && ok {
		sessionFiles = make(map[string]struct{}, len(ss.Files))
		for _, f := range ss.Files {
			sessionFiles[f.Path] = struct{}{}
		}
	}
	neighbors = reorderByRecencyAndTask(neighbors, seeds, sessionFiles, in.Task)
	if len(neighbors) > maxFiles {
		neighbors = neighbors[:maxFiles]
	}

	// Read each file, selecting mode based on budget ratio.
	var (
		files       []PrefetchFile
		tokensSpent int
	)
	for _, path := range neighbors {
		mode := pickMode(in.BudgetTokens, tokensSpent)
		content, tokens := s.readFileForPrefetch(ctx, in.ProjectRoot, path, mode)
		if content == "" {
			continue
		}
		files = append(files, PrefetchFile{
			Path:    path,
			Mode:    mode,
			Tokens:  tokens,
			Content: content,
		})
		tokensSpent += tokens
		// Stop early if budget exhausted.
		if in.BudgetTokens > 0 && tokensSpent >= in.BudgetTokens {
			break
		}
	}

	return nil, PrefetchOutput{
		Status:       "ok",
		Project:      p.Root,
		Total:        total,
		Files:        files,
		Seeds:        seeds,
		SkippedSeeds: skipped,
	}, nil
}

// resolveSeedPaths normalises paths to project-relative form, returning resolved
// paths and a list of paths that couldn't be mapped into the project tree.
func resolveSeedPaths(root string, rawPaths []string) (seeds, skipped []string) {
	seen := make(map[string]struct{})
	for _, raw := range rawPaths {
		p := raw
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			skipped = append(skipped, raw)
			continue
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		seeds = append(seeds, rel)
	}
	return seeds, skipped
}

// reorderByRecencyAndTask orders paths by a combined score:
//
//	score = recencyWeight + taskScore
//	  recencyWeight: 2 for seed files, 1 for session-touched files, 0 otherwise
//	  taskScore:     1 if any path segment matches a task keyword, 0 otherwise
//
// Seeds always surface first (recencyWeight=2 ensures no task mismatch can
// demote them below a task-relevant cold file). Session files come next, then
// cold files. Within each tier the original SpreadActivation order (activation
// energy) is preserved via a stable sort.
// sessionFiles and task may be nil/empty — the function degrades gracefully.
func reorderByRecencyAndTask(paths []string, seeds []string, sessionFiles map[string]struct{}, task string) []string {
	seedSet := make(map[string]struct{}, len(seeds))
	for _, s := range seeds {
		seedSet[s] = struct{}{}
	}

	taskWords := make(map[string]struct{})
	for _, w := range strings.Fields(strings.ToLower(task)) {
		if len(w) >= 3 {
			taskWords[w] = struct{}{}
		}
	}

	taskScore := func(p string) int {
		for _, seg := range strings.FieldsFunc(strings.ToLower(p), func(r rune) bool {
			return r == '/' || r == '_' || r == '.' || r == '-'
		}) {
			if _, ok := taskWords[seg]; ok {
				return 1
			}
		}
		return 0
	}

	score := func(p string) int {
		recency := 0
		if _, ok := seedSet[p]; ok {
			recency = 2
		} else if _, ok := sessionFiles[p]; ok {
			recency = 1
		}
		return recency + taskScore(p)
	}

	// Stable sort: highest combined score first, original order preserved within ties.
	out := make([]string, len(paths))
	copy(out, paths)
	// Bucket sort over score range [0, 3] — avoids allocating a sort.Slice closure
	// with a closure over the score map and preserves stability cheaply.
	buckets := [4][]string{}
	for _, p := range out {
		s := score(p)
		if s > 3 {
			s = 3
		}
		buckets[s] = append(buckets[s], p)
	}
	idx := 0
	for b := 3; b >= 0; b-- {
		copy(out[idx:], buckets[b])
		idx += len(buckets[b])
	}
	return out
}

// pickMode selects read fidelity based on remaining budget ratio.
// With no budget (0) always returns "signatures" — safe and fast.
func pickMode(budgetTokens, tokensSpent int) string {
	if budgetTokens <= 0 {
		return "signatures"
	}
	remaining := budgetTokens - tokensSpent
	ratio := float64(remaining) / float64(budgetTokens)
	switch {
	case ratio >= 0.8:
		return "full"
	case ratio >= 0.4:
		return "map"
	default:
		return "signatures"
	}
}

// readFileForPrefetch reads a single file at the requested fidelity and returns
// its content and an estimated token count. Falls back to "signatures" when full
// mode is unavailable (no chat model). Returns empty string on failure (caller skips).
func (s *Server) readFileForPrefetch(ctx context.Context, projectRoot, path, mode string) (content string, tokens int) {
	in := SummarizeInput{
		Path:        path,
		ProjectRoot: projectRoot,
		Mode:        mode,
	}
	out, err := s.Summarize(ctx, in)
	if err != nil || out.Status == "error" || out.Status == "chat-service-unreachable" {
		if mode == "full" {
			// Fall back to signatures — never skip a file just because chat is down.
			in.Mode = "signatures"
			out, err = s.Summarize(ctx, in)
			if err != nil || out.Status == "error" {
				return "", 0
			}
		} else {
			return "", 0
		}
	}
	c := out.Content
	return c, len(c) / 4 // rough chars-to-tokens estimate
}
