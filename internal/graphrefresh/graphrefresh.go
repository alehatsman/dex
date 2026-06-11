// Package graphrefresh refreshes the Go static call graph and embeds its
// nodes. It is the single implementation shared by the CLI index/watch
// paths (cmd/dex) and the MCP auto-watcher (internal/mcp), so the graph
// lane stays as fresh as the chunk lane on every reindex — the chunk run
// already releases the SQLite writer before either step fires.
package graphrefresh

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// RunPhase extracts the Go static graph for p and upserts it into st.
func RunPhase(ctx context.Context, p *proj.Project, st *store.Store, verbose bool, logger *slog.Logger) (*graph.Stats, error) {
	gx := graph.New(p, graph.NewStoreAdapter(st), graph.Options{
		Verbose: verbose,
		Logger:  logger,
	})
	return gx.Run(ctx)
}

// EmbedNodes embeds all graph_nodes whose vec_hash differs from
// content_hash (un-embedded or stale). Returns the number of nodes embedded.
func EmbedNodes(ctx context.Context, st *store.Store, em embed.Embedder, verbose bool) (int, error) {
	const batchSize = 256
	total := 0
	for {
		rows, err := st.NodesNeedingEmbed(ctx, batchSize)
		if err != nil {
			return total, fmt.Errorf("graphrefresh: fetch: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		texts := make([]string, len(rows))
		for i, r := range rows {
			texts[i] = r.EmbedText
		}
		vecs, err := em.Embed(ctx, texts)
		if err != nil {
			return total, fmt.Errorf("graphrefresh: embed: %w", err)
		}
		if err := st.SetNodeVecs(ctx, rows, vecs); err != nil {
			return total, fmt.Errorf("graphrefresh: set: %w", err)
		}
		total += len(rows)
		if verbose {
			fmt.Printf("  [graph-embed] embedded %d nodes (total %d)\n", len(rows), total)
		}
	}
	return total, nil
}
