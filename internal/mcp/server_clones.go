package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/store"
)

type ClonesInput struct {
	Path        string  `json:"path,omitempty" jsonschema:"restrict the scan to blocks under this relative path prefix (file or dir); empty scans the whole repo"`
	Threshold   float32 `json:"threshold,omitempty" jsonschema:"min cosine similarity for a duplicate edge, 0..1 (default 0.90)"`
	MinLines    int     `json:"min_lines,omitempty" jsonschema:"ignore blocks shorter than this many lines (default 6)"`
	K           int     `json:"k,omitempty" jsonschema:"neighbours probed per block (default 10, max 50)"`
	MaxClusters int     `json:"max_clusters,omitempty" jsonschema:"max clusters to return (default 20, max 100)"`
	ProjectRoot string  `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// CloneMemberOut is one code block inside a duplication cluster.
type CloneMemberOut struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
}

// CloneClusterOut is a set of near-duplicate blocks. Similarity is the weakest
// duplicate edge in the cluster; Size is the member count.
type CloneClusterOut struct {
	Size       int              `json:"size"`
	Similarity float32          `json:"similarity"`
	Members    []CloneMemberOut `json:"members"`
}

type ClonesOutput struct {
	Status   string            `json:"status"` // "ok" | "no-index" | "error"
	Hint     string            `json:"hint,omitempty"`
	Project  string            `json:"project,omitempty"`
	Clusters []CloneClusterOut `json:"clusters,omitempty"`
}

// Clones is the exported wrapper used by the REST surface.
func (s *Server) Clones(ctx context.Context, in ClonesInput) (ClonesOutput, error) {
	_, out, err := s.clones(ctx, nil, in)
	return out, err
}

// clones scans the indexed code blocks for clusters of semantically
// near-duplicate functions/methods — duplication hotspots for review/refactor.
// It reuses the vectors already indexed for search (sqlite-vec KNN), so it needs
// no embedder round-trip; an index built without embeddings simply yields none.
func (s *Server) clones(ctx context.Context, _ *sdk.CallToolRequest, in ClonesInput) (*sdk.CallToolResult, ClonesOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, ClonesOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, ClonesOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}

	k := in.K
	if k > 50 {
		k = 50
	}
	maxClusters := in.MaxClusters
	if maxClusters > 100 {
		maxClusters = 100
	}

	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, ClonesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	clusters, err := st.CloneClusters(ctx, store.CloneOpts{
		PathPrefix:  in.Path,
		Threshold:   in.Threshold,
		MinLines:    in.MinLines,
		K:           k,
		MaxClusters: maxClusters,
	})
	if err != nil {
		return nil, ClonesOutput{Status: "error", Hint: fmt.Sprintf("clones: %v", err)}, nil
	}

	out := ClonesOutput{Status: "ok", Project: p.Root}
	for _, c := range clusters {
		oc := CloneClusterOut{Size: len(c.Members), Similarity: c.Similarity}
		for _, m := range c.Members {
			oc.Members = append(oc.Members, CloneMemberOut{
				Path: m.Path, StartLine: m.StartLine, EndLine: m.EndLine, Kind: m.Kind, Name: m.Name,
			})
		}
		out.Clusters = append(out.Clusters, oc)
	}
	if len(out.Clusters) == 0 {
		out.Hint = "no near-duplicate blocks found above the threshold — lower `threshold` or `min_lines`, " +
			"or the index may lack embeddings (BM25-only)."
	}
	return nil, out, nil
}
