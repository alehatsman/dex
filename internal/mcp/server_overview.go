package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type OverviewInput struct {
	Task        string `json:"task" jsonschema:"task description used to rank files by relevance"`
	K           int    `json:"k,omitempty" jsonschema:"number of context files to surface (default 8, max 20)"`
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// OverviewFile is one file in the context set, ranked by relevance to the task.
type OverviewFile struct {
	Path          string  `json:"path"`
	Lines         int     `json:"lines,omitempty"`
	Score         float64 `json:"score"`
	SuggestedMode string  `json:"suggested_mode"` // "full" | "signatures" | "map"
}

type OverviewOutput struct {
	Status       string         `json:"status"` // "ok" | "partial" | "no-index" | "embedding-service-unreachable" | "error"
	Hint         string         `json:"hint,omitempty"`
	Project      string         `json:"project,omitempty"`
	Task         string         `json:"task,omitempty"`
	Context      []OverviewFile `json:"context,omitempty"`
	DistantCount int            `json:"distant_count"`
	Distant      []string       `json:"distant,omitempty"`
}

func (s *Server) Overview(ctx context.Context, in OverviewInput) (OverviewOutput, error) {
	_, out, err := s.overview(ctx, nil, in)
	return out, err
}

func (s *Server) overview(ctx context.Context, _ *sdk.CallToolRequest, in OverviewInput) (*sdk.CallToolResult, OverviewOutput, error) {
	if in.Task == "" {
		return nil, OverviewOutput{Status: "error", Hint: "task is empty"}, nil
	}
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, OverviewOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, OverviewOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k <= 0 {
		k = 8
	}
	if k > 20 {
		k = 20
	}

	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

	// Auto-load any context packages marked for automatic loading.
	// KnowledgeAdd is idempotent (UNIQUE body), so this is safe to call on every overview.
	if pkgs, err := st.PackList(ctx); err == nil {
		packDir := filepath.Join(p.CacheDir, "packages")
		for _, pkg := range pkgs {
			if !pkg.AutoLoad {
				continue
			}
			pkgPath := filepath.Join(packDir, pkg.Name+".ctxpkg")
			if loaded, loadErr := loadCtxPkg(pkgPath); loadErr == nil {
				for _, f := range loaded.Layers.Knowledge {
					if f.Body != "" {
						_, _ = st.KnowledgeAdd(ctx, f.Archetype, f.Body, f.Confidence)
					}
				}
			}
		}
	}

	// Line counts for all indexed code files — also used as the cold-start check.
	lineCounts, err := st.CodeFilePaths(ctx)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("file index: %v", err)}, nil
	}

	// N17: cold-start partial overview when index is still being populated.
	if len(lineCounts) == 0 {
		return overviewPartial(ctx, st, p)
	}

	// Embed the task to drive semantic ranking.
	vecs, err := s.EmbedClient.Embed(ctx, []string{in.Task})
	if err != nil {
		if errors.Is(err, embed.ErrUnreachable) {
			return nil, OverviewOutput{Status: "embedding-service-unreachable", Project: p.Root,
				Hint: "embedding service offline — overview requires embeddings; fall back to ask or grep."}, nil
		}
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("embed: %v", err)}, nil
	}
	vec := vecs[0]

	// Semantic search: pull more hits than k so we can aggregate by file.
	hits, err := st.Search(ctx, vec, in.Task, 50)
	if err != nil {
		return nil, OverviewOutput{Status: "error", Hint: fmt.Sprintf("search: %v", err)}, nil
	}

	// Aggregate: max score per file path.
	fileScore := make(map[string]float64, len(hits))
	for _, h := range hits {
		if h.Path == "" {
			continue
		}
		if sc := float64(h.Score); sc > fileScore[h.Path] {
			fileScore[h.Path] = sc
		}
	}

	// Per-file centrality (best-effort; skip if no graph).
	centrality, _ := st.FileCentrality(ctx)

	// Compute fused score: semantic × (1 + log1p(centrality)).
	type ranked struct {
		path  string
		score float64
	}
	scored := make([]ranked, 0, len(fileScore))
	for path, sem := range fileScore {
		c := centrality[path]
		scored = append(scored, ranked{path, sem * (1 + math.Log1p(c))})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].path < scored[j].path
	})

	// Build context set.
	contextSet := make(map[string]bool, k)
	ctxFiles := make([]OverviewFile, 0, k)
	for _, r := range scored {
		if len(ctxFiles) >= k {
			break
		}
		lc := lineCounts[r.path]
		ctxFiles = append(ctxFiles, OverviewFile{
			Path:          r.path,
			Lines:         lc,
			Score:         math.Round(r.score*1000) / 1000,
			SuggestedMode: suggestMode(lc),
		})
		contextSet[r.path] = true
	}

	// Distant: all indexed files not in context, sorted alphabetically.
	distant := make([]string, 0, len(lineCounts))
	for path := range lineCounts {
		if !contextSet[path] {
			distant = append(distant, path)
		}
	}
	sort.Strings(distant)

	const maxDistant = 100
	truncated := distant
	if len(truncated) > maxDistant {
		truncated = truncated[:maxDistant]
	}

	// Task classification hint.
	kind, scope := classifyTask(in.Task)
	hint = fmt.Sprintf("[TASK:%s SCOPE:%s] %s", kind, scope, outputHint(kind))

	// Wake-up knowledge briefing.
	facts, _ := st.KnowledgeTopForAsk(ctx, 5)
	if len(facts) > 0 {
		var kb strings.Builder
		kb.WriteString("\nKNOWLEDGE:\n")
		for _, f := range facts {
			fmt.Fprintf(&kb, "  [%s] %s\n", f.Archetype, f.Body)
		}
		hint += kb.String()
	}

	return nil, OverviewOutput{
		Status:       "ok",
		Project:      p.Root,
		Task:         in.Task,
		Context:      ctxFiles,
		DistantCount: len(distant),
		Distant:      truncated,
		Hint:         hint,
	}, nil
}

// classifyTask infers a coarse task kind and scope from the task description.
func classifyTask(task string) (kind, scope string) {
	lower := strings.ToLower(task)
	switch {
	case containsAny(lower, "fix", "bug", "error", "crash", "panic", "fail", "broken", "wrong", "incorrect"):
		kind = "fix"
	case containsAny(lower, "add", "implement", "create", "build", "write", "new feature", "support"):
		kind = "generate"
	case containsAny(lower, "refactor", "clean", "rename", "move", "extract", "restructure"):
		kind = "refactor"
	case containsAny(lower, "debug", "trace", "diagnose", "investigate", "why", "how does"):
		kind = "debug"
	case containsAny(lower, "test", "coverage", "spec"):
		kind = "test"
	case containsAny(lower, "doc", "comment", "readme", "explain"):
		kind = "docs"
	default:
		kind = "explore"
	}
	words := strings.Fields(task)
	switch {
	case len(words) <= 3:
		scope = "narrow"
	case len(words) <= 8:
		scope = "medium"
	default:
		scope = "broad"
	}
	return
}

func containsAny(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// outputHint returns a concise read-strategy hint for a task kind.
func outputHint(kind string) string {
	switch kind {
	case "fix":
		return "read callers → locate root cause → minimal patch"
	case "generate":
		return "read interfaces → implement → add tests"
	case "refactor":
		return "read full file → verify callers → edit → test"
	case "debug":
		return "trace call path → add logging → reproduce"
	case "test":
		return "read implementation → write table-driven tests"
	case "docs":
		return "read exported symbols → write concise docs"
	default:
		return "orient → read context files → reason"
	}
}

// overviewPartial returns a useful partial response when the chunk index is
// empty (cold start / indexing in progress). It scans the filesystem for
// project-type markers, emits a depth-2 directory tree, and surfaces any
// persistent knowledge facts — enough to start reasoning without a full index.
func overviewPartial(ctx context.Context, st *store.Store, p *proj.Project) (*sdk.CallToolResult, OverviewOutput, error) {
	facts, _ := st.KnowledgeTopForAsk(ctx, 5)
	markers, tree := projectMarkersAndTree(p.Root)

	var b strings.Builder
	b.WriteString("INDEXING IN PROGRESS — partial overview from filesystem scan.\n\n")
	if len(markers) > 0 {
		b.WriteString("Project markers: ")
		b.WriteString(strings.Join(markers, ", "))
		b.WriteString("\n\n")
	}
	if len(tree) > 0 {
		b.WriteString("Directory tree (depth 2):\n")
		for _, entry := range tree {
			b.WriteString(entry)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(facts) > 0 {
		b.WriteString("Project knowledge:\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "  [%s] %s\n", f.Archetype, f.Body)
		}
	}

	return nil, OverviewOutput{
		Status:  "partial",
		Project: p.Root,
		Hint:    b.String(),
	}, nil
}

// projectMarkersAndTree scans the project root to detect project-type markers
// and returns a depth-2 directory listing of non-hidden entries.
func projectMarkersAndTree(root string) (markers []string, tree []string) {
	knownMarkers := map[string]bool{
		"go.mod": true, "go.sum": true,
		"package.json": true, "package-lock.json": true, "yarn.lock": true,
		"Cargo.toml": true, "Cargo.lock": true,
		"pyproject.toml": true, "setup.py": true, "requirements.txt": true,
		"pom.xml": true, "build.gradle": true,
		"CMakeLists.txt": true,
		"Makefile":       true, "makefile": true,
		"Dockerfile": true,
		"CLAUDE.md":  true,
		"README.md":  true, "readme.md": true,
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && name != ".github" {
			continue
		}
		if knownMarkers[name] {
			markers = append(markers, name)
		}
		if e.IsDir() {
			tree = append(tree, "  "+name+"/")
			subEntries, err := os.ReadDir(filepath.Join(root, name))
			if err != nil {
				continue
			}
			shown := 0
			for _, se := range subEntries {
				if strings.HasPrefix(se.Name(), ".") {
					continue
				}
				if shown >= 10 {
					remaining := 0
					for _, rem := range subEntries[shown:] {
						if !strings.HasPrefix(rem.Name(), ".") {
							remaining++
						}
					}
					if remaining > 0 {
						tree = append(tree, fmt.Sprintf("    … +%d more", remaining))
					}
					break
				}
				if se.IsDir() {
					tree = append(tree, "    "+se.Name()+"/")
				} else {
					tree = append(tree, "    "+se.Name())
				}
				shown++
			}
		} else {
			tree = append(tree, "  "+name)
		}
	}
	return
}

// suggestMode returns the recommended file_view mode for a file of
// the given line count: short files can be read in full; large files are
// best approached via their symbol map.
func suggestMode(lines int) string {
	switch {
	case lines < 200:
		return "full"
	case lines < 800:
		return "signatures"
	default:
		return "map"
	}
}
