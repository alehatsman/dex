package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// openIndexedStore reindexes a one-file project and returns its root plus an
// open handle to the same DB the server reads, so a test can seed the bus.
func openIndexedStore(t *testing.T, srvURL, cacheDir, projDir string) (string, *store.Store) {
	t.Helper()
	root := indexProject(t, projDir, cacheDir, srvURL)
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return root, st
}

// seedFinding posts a peer finding embedded through the same fake embedder the
// server uses, so an identical question recalls it (hashVec is deterministic).
func seedFinding(t *testing.T, st *store.Store, srvURL, agentID, body string) {
	t.Helper()
	em := embed.New(srvURL, "fake", 16, 5*time.Second)
	vecs, err := em.Embed(context.Background(), []string{body})
	if err != nil || len(vecs) == 0 {
		t.Fatalf("embed seed: %v", err)
	}
	if _, err := st.AgentPostVec(context.Background(), agentID, "", "finding", body, vecs[0]); err != nil {
		t.Fatalf("post finding: %v", err)
	}
}

// TestFoldPeerFindings: a peer's finding surfaces in the ask pack tagged with
// its provenance; the asking agent's own finding is self-filtered out.
func TestFoldPeerFindings(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\nfunc Greet() string { return \"hi\" }\n")

	root, st := openIndexedStore(t, srv.URL, cacheDir, projDir)

	const q = "how is the assemble output bounded"
	seedFinding(t, st, srv.URL, "peer-A", q)            // a peer's finding
	seedFinding(t, st, srv.URL, "me", "my own finding") // ours — must be filtered

	s := newServer(srv.URL, cacheDir)
	s.AgentID = "me"

	var out ContextOutput
	s.foldPeerFindings(context.Background(), st, ContextInput{Question: q, ProjectRoot: root}, &out)

	var peer string
	for _, f := range out.KnowledgeFacts {
		if strings.Contains(f, "peer-agent:peer-A") {
			peer = f
		}
		if strings.Contains(f, "peer-agent:me") {
			t.Errorf("own finding was not self-filtered: %q", f)
		}
	}
	if peer == "" {
		t.Fatalf("peer finding not folded; got %v", out.KnowledgeFacts)
	}
	if !strings.HasPrefix(peer, "[peer-agent:peer-A]") {
		t.Errorf("provenance tag missing/wrong: %q", peer)
	}
}
