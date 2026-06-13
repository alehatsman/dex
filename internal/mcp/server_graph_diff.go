package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// reValidRef matches the characters legal in a git ref. It intentionally
// excludes '-' at the start (which git would interpret as a flag) and all
// characters that have no meaning in ref names.
var reValidRef = regexp.MustCompile(`^[a-zA-Z0-9~^:./_@{}-]+$`)

// ─── tool: graph_diff ─────────────────────────────────────────────────────

type DiffInput struct {
	Ref         string `json:"ref,omitempty" jsonschema:"git ref to diff against (default 'HEAD~1'); supports any ref git understands"`
	MaxDepth    int    `json:"max_depth,omitempty" jsonschema:"BFS depth for blast-radius traversal (default 2, max 5)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

type DiffOutput struct {
	Status       string       `json:"status"` // "ok" | "no-index" | "no-graph" | "no-changes" | "error"
	Hint         string       `json:"hint,omitempty"`
	Project      string       `json:"project,omitempty"`
	Ref          string       `json:"ref,omitempty"`
	ChangedFiles []string     `json:"changed_files,omitempty"`
	MaxDepth     int          `json:"max_depth,omitempty"`
	Total        int          `json:"total"`
	Truncated    bool         `json:"truncated,omitempty"`
	Nodes        []ImpactNode `json:"nodes,omitempty"`
}

func (s *Server) GraphDiff(ctx context.Context, in DiffInput) (DiffOutput, error) {
	_, out, err := s.graphDiff(ctx, nil, in)
	return out, err
}

func (s *Server) graphDiff(ctx context.Context, _ *sdk.CallToolRequest, in DiffInput) (*sdk.CallToolResult, DiffOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, DiffOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, DiffOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		ref = "HEAD~1"
	}
	if !reValidRef.MatchString(ref) {
		return nil, DiffOutput{Status: "error",
			Hint: fmt.Sprintf("invalid ref %q — only alphanumeric, ~^:./_@{}- characters allowed", ref)}, nil
	}
	maxDepth := in.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxDepth > 5 {
		maxDepth = 5
	}

	// Run git diff to collect changed files relative to the project root.
	changedFiles, err := gitDiffFiles(p.Root, ref)
	if err != nil {
		return nil, DiffOutput{Status: "error", Project: p.Root,
			Hint: fmt.Sprintf("git diff --name-only %s: %v", ref, err)}, nil
	}
	if len(changedFiles) == 0 {
		return nil, DiffOutput{Status: "no-changes", Project: p.Root, Ref: ref,
			Hint: fmt.Sprintf("no files changed between %s and HEAD", ref)}, nil
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, DiffOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, DiffOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, DiffOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	// Collect all graph nodes whose file path matches one of the changed files.
	changedSet := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = true
		// Also try relative path without leading ./
		changedSet[strings.TrimPrefix(f, "./")] = true
	}
	var seeds []graphNode
	seen := map[string]bool{}
	for _, n := range view.nodesByID {
		rel := n.FilePath
		if !changedSet[rel] {
			continue
		}
		if seen[n.ID] {
			continue
		}
		// Only seed on function/method symbols — not imports or types.
		if n.Kind != graph.NodeFunction && n.Kind != graph.NodeMethod {
			continue
		}
		seen[n.ID] = true
		seeds = append(seeds, n)
	}

	const maxBlastNodes = 300
	nodes := computeImpactNodes(view, seeds, maxDepth)

	out := DiffOutput{
		Status: "ok", Project: p.Root, Ref: ref,
		ChangedFiles: changedFiles,
		MaxDepth:     maxDepth,
		Total:        len(nodes),
	}
	if len(nodes) > maxBlastNodes {
		nodes = nodes[:maxBlastNodes]
		out.Truncated = true
	}
	out.Nodes = nodes
	return nil, out, nil
}

// gitDiffFiles runs `git diff --name-only <ref> HEAD` in root and returns
// the list of changed file paths relative to root.
func gitDiffFiles(root, ref string) ([]string, error) {
	mkCmd := func(args ...string) *exec.Cmd {
		c := exec.Command("git", args...) // #nosec G204
		c.Dir = root
		return c
	}
	out, err := mkCmd("diff", "--name-only", "--end-of-options", ref, "HEAD").Output()
	if err != nil {
		// Try without HEAD in case HEAD == ref (initial commit, detached HEAD)
		if out2, err2 := mkCmd("diff", "--name-only", "--end-of-options", ref).Output(); err2 == nil {
			out = out2
		} else {
			return nil, err
		}
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
