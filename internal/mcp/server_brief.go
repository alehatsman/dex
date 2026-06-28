package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// BriefInput is the input to the brief tool.
type BriefInput struct {
	Task         string   `json:"task" jsonschema:"task description — what you are about to implement or change"`
	Sections     []string `json:"sections,omitempty" jsonschema:"sections to include: map, relevant_code, rules, tests, impact (default: all)"`
	BudgetTokens int      `json:"budget_tokens,omitempty" jsonschema:"approximate token budget (default 6000)"`
	ProjectRoot  string   `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// BriefRelevantFile is one ranked file in the brief output.
type BriefRelevantFile struct {
	Path  string  `json:"path"`
	Score float32 `json:"score"`
	Mode  string  `json:"mode"`
	Why   string  `json:"why,omitempty"`
}

// BriefSymbol is one relevant symbol in the brief output.
type BriefSymbol struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Why       string `json:"why,omitempty"`
}

// BriefOutput is the output of the brief tool.
type BriefOutput struct {
	Status          string              `json:"status"` // "ok" | "no-index" | "no-embed" | "error"
	Hint            string              `json:"hint,omitempty"`
	Task            string              `json:"task"`
	Summary         string              `json:"summary,omitempty"`
	RelevantFiles   []BriefRelevantFile `json:"relevant_files,omitempty"`
	RelevantSymbols []BriefSymbol       `json:"relevant_symbols,omitempty"`
	LocalRules      []string            `json:"local_rules,omitempty"`
	Tests           []string            `json:"tests,omitempty"`
	Impact          []string            `json:"impact,omitempty"`
	Risks           []string            `json:"risks,omitempty"`
	Confidence      float32             `json:"confidence,omitempty"`
	TokenEstimate   int                 `json:"token_estimate,omitempty"`
	IndexStatus     *IndexStatusOutput  `json:"index_status,omitempty"`
	NextCalls       []briefNextCall     `json:"next_calls,omitempty"`
}

// briefNextCall is a suggested follow-up call in the brief output.
type briefNextCall struct {
	Tool   string `json:"tool"`
	Args   any    `json:"args,omitempty"`
	Reason string `json:"reason"`
}

// brief builds a task-specific context pack.
func (s *Server) brief(ctx context.Context, req *sdk.CallToolRequest, in BriefInput) (*sdk.CallToolResult, BriefOutput, error) {
	// 1. Resolve project root.
	root := in.ProjectRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, BriefOutput{Status: "error", Hint: "could not determine project root; pass project_root explicitly"}, nil
		}
		root = wd
	}

	// 2. Check index status.
	idxStatus, _ := s.IndexStatus(ctx, IndexStatusInput{ProjectRoot: root})
	if idxStatus.Status == "no-index" {
		return nil, BriefOutput{
			Status:      "no-index",
			Hint:        idxStatus.Hint,
			Task:        in.Task,
			IndexStatus: &idxStatus,
		}, nil
	}
	if idxStatus.Status == "error" {
		return nil, BriefOutput{
			Status:      "error",
			Hint:        idxStatus.Hint,
			Task:        in.Task,
			IndexStatus: &idxStatus,
		}, nil
	}

	// 3. Embed required for task-scored files.
	if s.EmbedClient == nil {
		return nil, BriefOutput{
			Status:      "no-embed",
			Hint:        "brief requires an embed client — run dex with DEX_EMBED_URL set",
			Task:        in.Task,
			IndexStatus: &idxStatus,
		}, nil
	}

	out := BriefOutput{
		Status:      "ok",
		Task:        in.Task,
		IndexStatus: &idxStatus,
	}

	// 4. Run taskMap for ranked file list.
	_, mapOut, _ := s.taskMap(ctx, MapInput{Task: in.Task, ProjectRoot: root})
	var l0Paths []string
	for _, f := range mapOut.L0Files {
		out.RelevantFiles = append(out.RelevantFiles, BriefRelevantFile{
			Path:  f.Path,
			Score: f.Score,
			Mode:  f.Mode,
		})
		l0Paths = append(l0Paths, f.Path)
	}
	for _, f := range mapOut.L1Files {
		out.RelevantFiles = append(out.RelevantFiles, BriefRelevantFile{
			Path:  f.Path,
			Score: f.Score,
			Mode:  f.Mode,
		})
	}

	// 5. Run semantic search for relevant symbols.
	k := 12
	searchOut, _ := s.Search(ctx, SearchInput{Query: in.Task, ProjectRoot: root, K: k})
	for _, h := range searchOut.Hits {
		out.RelevantSymbols = append(out.RelevantSymbols, BriefSymbol{
			Path:      h.Path,
			StartLine: h.StartLine,
			EndLine:   h.EndLine,
			Kind:      h.Kind,
		})
	}

	// 6. Scan project root for local rules files.
	out.LocalRules = collectLocalRules(root)

	// 7. Collect sibling test files for L0 relevant files.
	out.Tests = collectTestFiles(root, l0Paths)

	// 8. Set confidence: high when embed available.
	out.Confidence = 0.8

	// 9. Rough token estimate (4 chars ~= 1 token).
	total := len(in.Task)
	for _, f := range out.RelevantFiles {
		total += len(f.Path) + 20
	}
	for _, r := range out.LocalRules {
		total += len(r) + 10
	}
	out.TokenEstimate = total / 4

	// 10. Suggest next calls.
	if len(l0Paths) > 0 {
		out.NextCalls = []briefNextCall{
			{
				Tool:   "read",
				Args:   map[string]any{"paths": l0Paths},
				Reason: "read the top-ranked files before editing",
			},
		}
	}

	return nil, out, nil
}

// collectLocalRules scans the project root for rule/spec files (first 100 lines).
func collectLocalRules(root string) []string {
	var rules []string

	// Check known root-level rule files.
	candidates := []string{
		"CLAUDE.md",
		".dex/rules.md",
	}
	for _, name := range candidates {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			rules = append(rules, name)
		}
	}

	// Check docs/*.md.
	docsDir := filepath.Join(root, "docs")
	if entries, err := os.ReadDir(docsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				rules = append(rules, filepath.Join("docs", e.Name()))
			}
		}
	}

	// Check specs/*.md.
	specsDir := filepath.Join(root, "specs")
	if entries, err := os.ReadDir(specsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				rules = append(rules, filepath.Join("specs", e.Name()))
			}
		}
	}

	return rules
}

// collectTestFiles returns sibling *_test.go files for the given L0 paths.
func collectTestFiles(root string, paths []string) []string {
	seen := make(map[string]bool)
	var tests []string

	for _, p := range paths {
		dir := filepath.Dir(filepath.Join(root, p))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			rel, err := filepath.Rel(root, filepath.Join(dir, e.Name()))
			if err != nil {
				rel = fmt.Sprintf("%s/%s", filepath.Dir(p), e.Name())
			}
			if !seen[rel] {
				seen[rel] = true
				tests = append(tests, rel)
			}
		}
	}
	return tests
}
