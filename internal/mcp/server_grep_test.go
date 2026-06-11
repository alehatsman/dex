package mcp

import (
	"context"
	"testing"
)

// TestSearchGrepNotFound asserts that an explicit but non-existent subdir
// path fails loud (status "not-found") instead of silently walking the whole
// project root and returning a misleading "no-matches" (issue #73).
func TestSearchGrepNotFound(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	_, out, err := s.searchGrep(context.Background(), nil, SearchGrepInput{
		Pattern:     "foo",
		Path:        "does/not/exist",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "not-found" {
		t.Errorf("status = %q, want not-found", out.Status)
	}
}
