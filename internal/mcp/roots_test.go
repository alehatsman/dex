package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubLister is a fake MCP client for root resolution: it returns canned roots
// (or an error) and records whether ListRoots was called.
type stubLister struct {
	roots  []*sdk.Root
	err    error
	called bool
}

func (s *stubLister) ListRoots(_ context.Context, _ *sdk.ListRootsParams) (*sdk.ListRootsResult, error) {
	s.called = true
	if s.err != nil {
		return nil, s.err
	}
	return &sdk.ListRootsResult{Roots: s.roots}, nil
}

func fileURI(path string) string { return "file://" + path }

// sameDir compares two paths by their symlink-resolved form; macOS temp dirs
// live under /var → /private/var, which proj.Resolve canonicalizes.
func sameDir(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

func TestFileURIToPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"file:///a/b", "/a/b"},
		{"/a/b", "/a/b"},
		{"http://x/y", ""},
		{"", ""},
		{"file://", ""},
	}
	for _, c := range cases {
		if got := fileURIToPath(c.in); got != c.want {
			t.Errorf("fileURIToPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRootFromClientPicksWorktree is the coverage #120 requires: an omitted
// project_root resolves to the client's declared worktree, not the server cwd.
func TestRootFromClientPicksWorktree(t *testing.T) {
	wt := t.TempDir()
	base := t.TempDir()
	stub := &stubLister{roots: []*sdk.Root{{URI: fileURI(wt)}}}

	if got := rootFromClient(context.Background(), stub, base); !sameDir(got, wt) {
		t.Fatalf("rootFromClient = %q, want %q", got, wt)
	}

	s := &Server{IndexDir: base}
	p, hint := s.resolveProject(withLister(context.Background(), stub), "")
	if hint != "" {
		t.Fatalf("resolveProject hint = %q", hint)
	}
	if !sameDir(p.Root, wt) {
		t.Errorf("resolved root = %q, want worktree %q", p.Root, wt)
	}
}

func TestRootFromClientDegradations(t *testing.T) {
	base := t.TempDir()
	t.Run("empty roots", func(t *testing.T) {
		if got := rootFromClient(context.Background(), &stubLister{}, base); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("list error", func(t *testing.T) {
		l := &stubLister{err: errors.New("unsupported")}
		if got := rootFromClient(context.Background(), l, base); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("non-file uri", func(t *testing.T) {
		l := &stubLister{roots: []*sdk.Root{{URI: "http://x/y"}}}
		if got := rootFromClient(context.Background(), l, base); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("nonexistent dir", func(t *testing.T) {
		l := &stubLister{roots: []*sdk.Root{{URI: fileURI(filepath.Join(base, "absent"))}}}
		if got := rootFromClient(context.Background(), l, base); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestResolveProjectPrecedence(t *testing.T) {
	base := t.TempDir()
	arg := t.TempDir()
	rootDir := t.TempDir()
	s := &Server{IndexDir: base}
	stub := &stubLister{roots: []*sdk.Root{{URI: fileURI(rootDir)}}}
	ctx := withLister(context.Background(), stub)

	// 1. explicit arg wins, and roots is never consulted.
	p, hint := s.resolveProject(ctx, arg)
	if hint != "" {
		t.Fatalf("arg: hint = %q", hint)
	}
	if !sameDir(p.Root, arg) {
		t.Errorf("arg: root = %q, want %q", p.Root, arg)
	}
	if stub.called {
		t.Error("arg present but ListRoots was called")
	}

	// 2. omitted arg → client root wins over cwd.
	stub.called = false
	p, hint = s.resolveProject(ctx, "")
	if hint != "" {
		t.Fatalf("roots: hint = %q", hint)
	}
	if !sameDir(p.Root, rootDir) {
		t.Errorf("roots: root = %q, want client root %q", p.Root, rootDir)
	}
	if !stub.called {
		t.Error("omitted arg but ListRoots was not called")
	}

	// 3. no session → cwd backstop (must not be the fake client root).
	p, hint = s.resolveProject(context.Background(), "")
	if hint != "" {
		t.Fatalf("cwd: hint = %q", hint)
	}
	if sameDir(p.Root, rootDir) {
		t.Error("no session but resolved to the client root")
	}
}

// TestProjectRootDescNoStaleCwdDefault guards pillar B: the misleading
// "defaults to the server's working directory" phrasing must not creep back,
// and the worktree guidance must be present.
func TestProjectRootDescNoStaleCwdDefault(t *testing.T) {
	f, ok := reflect.TypeOf(SearchInput{}).FieldByName("ProjectRoot")
	if !ok {
		t.Fatal("SearchInput has no ProjectRoot field")
	}
	tag := f.Tag.Get("jsonschema")
	if strings.Contains(tag, "defaults to the server's working directory") {
		t.Errorf("stale cwd-default project_root description: %q", tag)
	}
	if !strings.Contains(tag, "worktree") {
		t.Errorf("project_root description lacks worktree guidance: %q", tag)
	}
}
