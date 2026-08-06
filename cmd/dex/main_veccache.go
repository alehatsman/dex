package main

import (
	"io"
	"path/filepath"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/veccache"
)

// indexEmbedder builds the embedder for an indexing pass, wrapping the live
// embed client with a project-scoped content-addressed vector cache so a
// reindex reuses vectors for unchanged content instead of re-embedding — the
// dominant cost of a full rebuild (#121). The cache is best-effort: any open
// failure yields the unwrapped client, so indexing always proceeds.
//
// The returned embedder is nil in the lean/none profile, exactly like
// newEmbedClient. The io.Closer is nil when there is nothing to close;
// otherwise the caller must close it when the indexing pass is done.
func indexEmbedder(p *proj.Project, indexModel string) (embed.Embedder, io.Closer) {
	em := newEmbedClient(indexModel)
	if em == nil {
		return nil, nil
	}
	vc, err := veccache.Open(
		filepath.Join(p.CacheDir, veccache.FileName),
		veccache.MaxRowsFromEnv())
	if err != nil {
		return em, nil
	}
	return embed.WithCache(em, vc), vc
}
