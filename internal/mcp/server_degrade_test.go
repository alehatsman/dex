package mcp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
)

// closedURL returns an http:// URL pointing at a port guaranteed to refuse
// connections (listen :0, record addr, close). Safer than hardcoded :1 which
// may be occupied on some environments (e.g. WSL2).
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("closedURL: listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

// degradedServer indexes projDir with a working embed backend, then returns a
// Server whose EmbedClient points at a closed port so every query-time Embed
// call returns embed.ErrUnreachable — simulating the embedding service going
// offline after the index was already built.
func degradedServer(t *testing.T, projDir, cacheDir string) (*Server, string) {
	t.Helper()
	embedSrv := fakeEmbed(t, 16)
	root := indexProject(t, projDir, cacheDir, embedSrv.URL)
	embedSrv.Close() // backend goes away after indexing

	s := &Server{
		EmbedClient: embed.New(closedURL(t), "fake", 16, 200*time.Millisecond),
		IndexDir:    cacheDir,
	}
	return s, root
}

// TestDegradeEmbedBackendDown locks the serve/search-time graceful-degradation
// contract (issue #175): when the embedding backend is unreachable, the
// embedding-dependent tools return status "embedding-service-unreachable" with
// NO error — never a hard crash — and the embedding-independent tools keep
// working off the existing index.
//
// If a refactor lets an embed outage surface as a hard error (non-nil error or
// status "error") on any of these tools, this test fails.
func TestDegradeEmbedBackendDown(t *testing.T) {
	projDir := t.TempDir()
	writeFile(t, projDir+"/auth.go", "package x\n\n// Authenticate validates a token.\nfunc Authenticate(token string) error {\n\tif token == \"\" {\n\t\treturn nil\n\t}\n\treturn nil\n}\n")
	writeFile(t, projDir+"/math.go", "package x\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	cacheDir := t.TempDir()

	s, root := degradedServer(t, projDir, cacheDir)
	ctx := context.Background()

	// --- embedding-dependent tools: must degrade, not error ---
	t.Run("search", func(t *testing.T) {
		_, out, err := s.search(ctx, nil, SearchInput{Query: "authenticate token", ProjectRoot: root})
		if err != nil {
			t.Fatalf("search returned hard error on embed outage: %v", err)
		}
		if out.Status != "embedding-service-unreachable" {
			t.Errorf("search status = %q, want embedding-service-unreachable", out.Status)
		}
	})

	// --- embedding-independent tools: must keep working off the index ---
	t.Run("search_symbol unaffected", func(t *testing.T) {
		_, out, err := s.findSymbol(ctx, nil, FindSymbolInput{Name: "Authenticate", ProjectRoot: root})
		if err != nil {
			t.Fatalf("search_symbol errored during embed outage: %v", err)
		}
		if out.Status != "ok" {
			t.Errorf("search_symbol status = %q, want ok (must not depend on embeddings)", out.Status)
		}
	})

	t.Run("search_grep unaffected", func(t *testing.T) {
		_, out, err := s.searchGrep(ctx, nil, SearchGrepInput{Pattern: "Authenticate", ProjectRoot: root})
		if err != nil {
			t.Fatalf("search_grep errored during embed outage: %v", err)
		}
		if out.Status != "ok" {
			t.Errorf("search_grep status = %q, want ok (must not depend on embeddings)", out.Status)
		}
	})
}
