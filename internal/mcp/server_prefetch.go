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

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, PrefetchOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

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

	// Score by task-keyword overlap when task is given.
	if in.Task != "" {
		neighbors = reorderByTaskRelevance(neighbors, in.Task)
	}
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

// reorderByTaskRelevance moves files whose path components overlap with task
// keywords to the front. Simple heuristic — no embedding required.
func reorderByTaskRelevance(paths []string, task string) []string {
	taskWords := make(map[string]struct{})
	for _, w := range strings.Fields(strings.ToLower(task)) {
		if len(w) >= 3 {
			taskWords[w] = struct{}{}
		}
	}
	if len(taskWords) == 0 {
		return paths
	}
	score := func(p string) int {
		s := 0
		for _, seg := range strings.FieldsFunc(strings.ToLower(p), func(r rune) bool {
			return r == '/' || r == '_' || r == '.' || r == '-'
		}) {
			if _, ok := taskWords[seg]; ok {
				s++
			}
		}
		return s
	}
	// Stable partition: scored paths first, then unscored, order preserved within each group.
	var hi, lo []string
	for _, p := range paths {
		if score(p) > 0 {
			hi = append(hi, p)
		} else {
			lo = append(lo, p)
		}
	}
	return append(hi, lo...)
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
