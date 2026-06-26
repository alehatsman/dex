package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// checkFixture builds a tiny index with two named chunks in one file.
func checkFixture(t *testing.T) (*Server, string) {
	t.Helper()
	idxDir := t.TempDir()
	projDir := t.TempDir()

	p, err := proj.Resolve(projDir, idxDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMany(context.Background(), []store.PendingChunk{
		{Path: "main.go", Kind: "fn", Name: "Foo", StartLine: 1, EndLine: 5, ContentSHA: "h1", Content: "func Foo(){}"},
		{Path: "main.go", Kind: "fn", Name: "Bar", StartLine: 10, EndLine: 15, ContentSHA: "h2", Content: "func Bar(){}"},
	}, time.Now()); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_ = st.Close()

	s := &Server{IndexDir: idxDir}
	return s, projDir
}

func TestCheckOK(t *testing.T) {
	s, root := checkFixture(t)
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: "main.go:3:Foo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	r := out.Results[0]
	if r.Status != "ok" {
		t.Errorf("status = %q, want ok (symbol_at=%q)", r.Status, r.SymbolAt)
	}
}

func TestCheckGone(t *testing.T) {
	s, root := checkFixture(t)
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: "main.go:99:Missing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Results[0]
	if r.Status != "gone" {
		t.Errorf("status = %q, want gone", r.Status)
	}
}

func TestCheckMoved(t *testing.T) {
	s, root := checkFixture(t)
	// Ref says "Foo" at line 10 (where Bar lives) — symbol is moved.
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: "main.go:10:Foo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Results[0]
	if r.Status != "moved" {
		t.Errorf("status = %q, want moved (found_at=%q)", r.Status, r.FoundAt)
	}
	if r.FoundAt == "" {
		t.Error("found_at should be set when moved")
	}
}

func TestCheckNoFile(t *testing.T) {
	s, root := checkFixture(t)
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: "nonexistent.go:1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Results[0]
	if r.Status != "no_file" {
		t.Errorf("status = %q, want no_file", r.Status)
	}
}

func TestCheckParseError(t *testing.T) {
	s, root := checkFixture(t)
	// Empty ref → parse_error
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: ""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.Results[0]
	if r.Status != "parse_error" {
		t.Errorf("status = %q, want parse_error", r.Status)
	}
}

// TestCheckAbsolutePath: an absolute path inside the project is rebased to
// project-relative before the index lookup, matching the locate/claims behavior.
func TestCheckAbsolutePath(t *testing.T) {
	s, root := checkFixture(t)
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: root + "/main.go:3:Foo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	r := out.Results[0]
	if r.Status != "ok" {
		t.Errorf("absolute-path claim: status = %q, want ok (symbol_at=%q)", r.Status, r.SymbolAt)
	}
	if r.Ref != root+"/main.go:3:Foo" {
		t.Errorf("original ref not echoed: got %q", r.Ref)
	}
}

// TestCheckDoesNotCloseCachedStore pins #712: check must not close the shared
// cached store handle. Before the fix, defer st.Close() in check() closed the
// handle returned by s.openStore (cached via sync.Once). A second call to check
// on the same Server would receive the already-closed handle and return
// "sql: database is closed".
func TestCheckDoesNotCloseCachedStore(t *testing.T) {
	s, root := checkFixture(t)
	in := CheckInput{
		ProjectRoot: root,
		Claims:      []ClaimRef{{Ref: "main.go:3:Foo"}},
	}
	// First call warms the cache.
	_, out, err := s.check(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if out.Results[0].Status != "ok" {
		t.Fatalf("first call: status = %q, want ok", out.Results[0].Status)
	}
	// Second call must succeed — if the cache was poisoned the store is closed.
	_, out, err = s.check(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("second call (cache poisoned?): %v", err)
	}
	if out.Results[0].Status != "ok" {
		t.Errorf("second call: status = %q, want ok", out.Results[0].Status)
	}
}

func TestCheckBatch(t *testing.T) {
	s, root := checkFixture(t)
	_, out, err := s.check(context.Background(), nil, CheckInput{
		ProjectRoot: root,
		Claims: []ClaimRef{
			{Ref: "main.go:1:Foo"},
			{Ref: "main.go:99"},
			{Ref: "nope.go:1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(out.Results))
	}
	statuses := []string{out.Results[0].Status, out.Results[1].Status, out.Results[2].Status}
	want := []string{"ok", "gone", "no_file"}
	for i, s := range statuses {
		if s != want[i] {
			t.Errorf("result[%d] status = %q, want %q", i, s, want[i])
		}
	}
}
