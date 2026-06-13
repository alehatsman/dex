package store

import "context"

// Searcher is the read-only retrieval surface of a Store. Handlers that only
// query the index (semantic search, symbol lookup, chunk retrieval) accept a
// Searcher instead of a *Store so they are decoupled from persistence and can
// be tested with a stub.
//
// *Store satisfies Searcher — pass one directly wherever a Searcher is needed.
type Searcher interface {
	Search(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error)
	SearchFused(ctx context.Context, queryVec []float32, queryText string, k int) ([]Hit, error)
	FindSymbol(ctx context.Context, name string, k int) ([]Hit, error)
	FindSymbolCandidates(ctx context.Context, query string, k int) ([]string, error)
	RelatedChunks(ctx context.Context, path string, startLine, k int) ([]Hit, error)
	ChunkAt(ctx context.Context, path string, startLine int) (Hit, error)
}

// compile-time check: *Store must implement Searcher.
var _ Searcher = (*Store)(nil)
