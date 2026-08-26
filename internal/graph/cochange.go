package graph

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/alehatsman/dex/internal/gitenv"
	"github.com/alehatsman/dex/internal/gitlog"
)

// Co-change mining parameters (#212). Kept as consts, not config, for v1 —
// see coChangeEnabled for the one toggle that exists.
const (
	// coChangeMaxCommits caps the git-log walk. Bounds mining cost on repos
	// with very long history; recent history carries the strongest signal
	// anyway.
	coChangeMaxCommits = 2000
	// coChangeMaxFilesPerCommit drops commits touching more files than this
	// as noise (mass-rename/vendor-bump/formatting commits, not coupling
	// signal). Pairing is O(k^2) in files-per-commit, so this also bounds
	// per-commit mining cost.
	coChangeMaxFilesPerCommit = 30
	// coChangeMinSupport is the minimum number of commits a file pair must
	// co-occur in to be considered.
	coChangeMinSupport = 2
	// coChangeMinConfidence is the minimum confidence (support / min of the
	// two files' individual commit counts) a pair must clear.
	coChangeMinConfidence = 0.1
)

// coChangeEnabled reports whether git-history co-change mining runs during
// graph indexing. On by default: the pass is bounded (coChangeMaxCommits,
// coChangeMaxFilesPerCommit) and best-effort (mineCoChanges never fails
// indexing), so it's cheap enough to run unconditionally. Set
// DEX_GRAPH_COCHANGE=0 to disable — e.g. a non-git root, a shallow clone
// with no usable log, or a repo with unwanted-huge history.
func coChangeEnabled() bool { return os.Getenv("DEX_GRAPH_COCHANGE") != "0" }

// filePair is an unordered pair of repo-relative file paths, canonicalized
// so a < b — the map key for pair counting.
type filePair struct{ a, b string }

func newFilePair(a, b string) filePair {
	if a > b {
		a, b = b, a
	}
	return filePair{a, b}
}

// coChangeCache holds the last successful mine per repo root, keyed by the
// HEAD commit it was mined at plus the node count it was mined against.
// Watch-mode reindexes fire on every debounced file save, and most saves
// don't move HEAD at all, so a same-process rerun with an unchanged HEAD
// (and an unchanged node count — a cheap proxy for "no newly-extracted file
// entered the graph since") reuses the prior mine instead of re-walking up
// to coChangeMaxCommits commits and re-counting pairs. A HEAD/node-count
// change invalidates it; there is no cross-process persistence, so a fresh
// `dex index` process always re-mines once.
var (
	coChangeCacheMu sync.Mutex
	coChangeCache   = map[string]coChangeCacheEntry{}
)

type coChangeCacheEntry struct {
	head      string
	nodeCount int
	edges     []Edge
}

// mineCoChanges mines git history under root for file-level temporal
// coupling (#212) and returns co_changes edges between file nodes present in
// nodes. A non-git root or empty history is not an error (see
// gitlog.Collect) — legitimately nothing to mine. A genuine failure to run
// git (binary missing, context cancelled) IS returned as an error, so the
// caller can preserve previously-mined edges instead of letting an empty
// result prune them.
func mineCoChanges(ctx context.Context, root string, nodes []Node) ([]Edge, error) {
	head := headHash(ctx, root)
	cacheKey := root
	if head != "" {
		coChangeCacheMu.Lock()
		entry, ok := coChangeCache[cacheKey]
		coChangeCacheMu.Unlock()
		if ok && entry.head == head && entry.nodeCount == len(nodes) {
			return entry.edges, nil
		}
	}

	commits, err := gitlog.Collect(ctx, root, coChangeMaxCommits)
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, nil
	}
	edges := coChangeEdgesFromCommits(commits, nodes)

	if head != "" {
		coChangeCacheMu.Lock()
		coChangeCache[cacheKey] = coChangeCacheEntry{head: head, nodeCount: len(nodes), edges: edges}
		coChangeCacheMu.Unlock()
	}
	return edges, nil
}

// headHash returns the commit HEAD currently points at, or "" if root has no
// resolvable HEAD (empty repo, not a git root, git unrunnable) — callers
// treat "" as "cache doesn't apply here", not as an error; mineCoChanges'
// own gitlog.Collect call surfaces the real error, if any, on the same root.
func headHash(ctx context.Context, root string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = root
	cmd.Env = gitenv.Current()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// coChangeEdgesFromCommits is the pure pair-counting/support/confidence core
// of mineCoChanges, split out so the algorithm is testable against a
// hand-built commit log without shelling out to git.
func coChangeEdgesFromCommits(commits []gitlog.Commit, nodes []Node) []Edge {
	pairCount := map[filePair]int{}
	commitsTouching := map[string]int{}
	for _, c := range commits {
		if len(c.Files) > coChangeMaxFilesPerCommit {
			continue // noise commit — skip entirely, no counts contributed
		}
		for _, f := range c.Files {
			commitsTouching[f]++
		}
		for i := 0; i < len(c.Files); i++ {
			for j := i + 1; j < len(c.Files); j++ {
				if c.Files[i] == c.Files[j] {
					continue
				}
				pairCount[newFilePair(c.Files[i], c.Files[j])]++
			}
		}
	}

	// file path -> its file-level node ID, restricted to nodes actually
	// present in this run's extraction (so edges never dangle on a file the
	// graph never emitted a node for: deleted, gitignored, unsupported
	// extension, binary). Two node kinds represent "one node per file":
	// NodeFile (Go/YAML/sitter languages) and NodeDocument (markdown, see
	// ExtractMarkdown) — without NodeDocument, every co-change pair
	// involving a .md file (e.g. this repo's own specs/*.md changing
	// alongside its implementing .go file) would silently never get an
	// edge, even though both files exist in the graph.
	fileNodeID := map[string]string{}
	for _, n := range nodes {
		if (n.Kind == NodeFile || n.Kind == NodeDocument) && n.FilePath != "" {
			fileNodeID[n.FilePath] = n.ID
		}
	}

	var edges []Edge
	for pair, support := range pairCount {
		if support < coChangeMinSupport {
			continue
		}
		minTouch := commitsTouching[pair.a]
		if commitsTouching[pair.b] < minTouch {
			minTouch = commitsTouching[pair.b]
		}
		if minTouch == 0 {
			continue
		}
		confidence := float64(support) / float64(minTouch)
		if confidence < coChangeMinConfidence {
			continue
		}
		srcID, ok := fileNodeID[pair.a]
		if !ok {
			continue
		}
		dstID, ok := fileNodeID[pair.b]
		if !ok {
			continue
		}
		if srcID > dstID {
			srcID, dstID = dstID, srcID
		}
		edges = append(edges, Edge{
			ID:    EdgeID(srcID, EdgeCoChanges, dstID, "", 0),
			Kind:  EdgeCoChanges,
			SrcID: srcID,
			DstID: dstID,
			// Both stored as float64, not int: Metadata round-trips through
			// JSON when persisted (see marshalMetadata/unmarshalMetadata),
			// and encoding/json always decodes numbers into interface{} as
			// float64 — an int here would read back as float64 from the
			// store, silently breaking any `.({int})` type assertion by a
			// future store-reading consumer.
			Metadata: map[string]any{
				"support":    float64(support),
				"confidence": confidence,
			},
		})
	}

	// Deterministic order: pairCount iteration is map-random, and the same
	// pair count feeds an edge list an upsert consumes 1:1 — sort so runs
	// are reproducible and diffable.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].SrcID != edges[j].SrcID {
			return edges[i].SrcID < edges[j].SrcID
		}
		return edges[i].DstID < edges[j].DstID
	})

	return edges
}
