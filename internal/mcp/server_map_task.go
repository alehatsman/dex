package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/gitrecency"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskMapSearchK is the KNN limit for the semantic search leg. The vec0 ANN
// index surfaces the top-K chunks; grouping by file gives per-file max scores.
// Files outside the top-K score below detectable thresholds and fall into L2.
const taskMapSearchK = 300

// taskMapL0Threshold and taskMapL1Threshold define the L0/L1/L2 score buckets
// as documented in issue #609. Scores are cosine similarity in [–1, 1] after
// the git-recency and bounce boosts are applied.
const (
	taskMapL0Threshold = float32(0.8)
	taskMapL1Threshold = float32(0.3)
)

// taskMap implements map(task=...) — a task-scored read list (#609). It embeds
// the task, runs a wide semantic search, groups results by file, applies git-
// recency and session-bounce boosts, then buckets files into L0/L1/L2 with a
// per-file recommended_mode. Returns status=no-embed when no embedder is wired
// so callers know to fall back to the topology map.
func (s *Server) taskMap(ctx context.Context, in MapInput) (*sdk.CallToolResult, MapOutput, error) {
	if s.EmbedClient == nil {
		return nil, MapOutput{
			Status: "no-embed",
			Hint:   "task-filtered map requires an embed client — run dex with DEX_EMBED_URL set, or omit task for the topology map",
		}, nil
	}

	p, hint := s.resolveProject(ctx, in.ProjectRoot)
	if hint != "" {
		return nil, MapOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, MapOutput{
			Status: "no-index",
			Hint:   fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root),
		}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	// 1. Embed the task string.
	vecs, err := s.EmbedClient.Embed(ctx, []string{in.Task})
	if err != nil || len(vecs) == 0 {
		return nil, MapOutput{Status: "error", Hint: fmt.Sprintf("embed task: %v", err)}, nil
	}
	queryVec := vecs[0]

	// 2. Semantic search — wide K to maximise file coverage. SearchFused (not
	// Search) avoids the outer 5× candidateK multiplier that Search applies for
	// graph expansion and cross-encoder reranking; at file-level scoring those
	// stages add noise and risk hitting the vec0 KNN 4096-row cap.
	hits, err := st.SearchFused(ctx, queryVec, in.Task, taskMapSearchK)
	if err != nil {
		return nil, MapOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// 3. Group by file: max cosine score per path.
	fileScore := make(map[string]float32, len(hits))
	for _, h := range hits {
		if h.Path == "" {
			continue
		}
		if h.Score > fileScore[h.Path] {
			fileScore[h.Path] = h.Score
		}
	}

	// 4. Git-recency boost: reuse the TTL-cached instance already wired into the
	// store; fall back to a fresh cache only when the store was opened without
	// a git root (edge case: bare index opened by path, no project root).
	paths := make([]string, 0, len(fileScore))
	for p := range fileScore {
		paths = append(paths, p)
	}
	gc := s.storeGitRecency(p.DBPath)
	if gc == nil {
		gc = gitrecency.New(p.Root)
	}
	recency := gc.FileScores(paths)
	var gitBoosted []string
	for path, boost := range recency {
		if _, seen := fileScore[path]; seen {
			fileScore[path] += boost
			gitBoosted = append(gitBoosted, path)
		}
	}
	sort.Strings(gitBoosted)

	// 5. Session-bounce boost: files already read this session are clearly
	// relevant — give them a small additive bump.
	const bounceBoost = float32(0.05)
	if ss, ok, _ := st.SessionGet(ctx); ok {
		for _, f := range ss.Files {
			if _, seen := fileScore[f.Path]; seen {
				fileScore[f.Path] += bounceBoost
			}
		}
	}

	// 6. Bucket into L0 / L1 / L2. recommendMode picks the cheapest mode that
	// gives the agent useful signal: L0 files get full content; L1 get
	// signatures; L2 are just counted.
	var l0, l1 []TaskFile
	l2Count := 0
	for path, score := range fileScore {
		switch {
		case score >= taskMapL0Threshold:
			l0 = append(l0, TaskFile{Path: path, Score: score, Mode: recommendMode(path)})
		case score >= taskMapL1Threshold:
			l1 = append(l1, TaskFile{Path: path, Score: score, Mode: "signatures"})
		default:
			l2Count++
		}
	}
	sort.Slice(l0, func(i, j int) bool { return l0[i].Score > l0[j].Score })
	sort.Slice(l1, func(i, j int) bool { return l1[i].Score > l1[j].Score })

	// 7. Render a compact text digest (the `Map` field), so the output is
	// human-readable and plays nicely with the existing text pipeline.
	var b strings.Builder
	fmt.Fprintf(&b, "# Task-filtered read list\n\n**Task:** %s\n\n", in.Task)
	if len(l0) > 0 {
		b.WriteString("## L0 — read now (score ≥ 0.8)\n\n")
		for _, f := range l0 {
			fmt.Fprintf(&b, "- `%s` score=%.2f  mode=%s\n", f.Path, f.Score, f.Mode)
		}
		b.WriteString("\n")
	}
	if len(l1) > 0 {
		b.WriteString("## L1 — skim (score 0.3–0.8)\n\n")
		for _, f := range l1 {
			fmt.Fprintf(&b, "- `%s` score=%.2f  mode=%s\n", f.Path, f.Score, f.Mode)
		}
		b.WriteString("\n")
	}
	if l2Count > 0 {
		fmt.Fprintf(&b, "## L2 — %d file(s) below threshold (score < 0.3); skip unless forced\n\n", l2Count)
	}
	if len(gitBoosted) > 0 {
		fmt.Fprintf(&b, "_Git-recency boost applied to: %s_\n", strings.Join(gitBoosted, ", "))
	}

	return nil, MapOutput{
		Status:     "ok",
		Zoom:       "task",
		Map:        b.String(),
		Task:       in.Task,
		L0Files:    l0,
		L1Files:    l1,
		L2Count:    l2Count,
		GitBoosted: gitBoosted,
	}, nil
}

// recommendMode returns the recommended read mode for an L0 file. Files with a
// Go extension use "signatures" (the index has their symbols); others use "full"
// since the index may not cover their type system.
func recommendMode(path string) string {
	switch {
	case strings.HasSuffix(path, ".go"),
		strings.HasSuffix(path, ".ts"),
		strings.HasSuffix(path, ".tsx"),
		strings.HasSuffix(path, ".js"),
		strings.HasSuffix(path, ".py"),
		strings.HasSuffix(path, ".java"),
		strings.HasSuffix(path, ".rs"):
		return "signatures"
	default:
		return "full"
	}
}
