package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// locateFixture indexes a tiny Go project with a caller edge so the locate
// composition (resolution + callers + sibling tests) has real data to chew on.
func locateFixture(t *testing.T) (s *Server, root string) {
	t.Helper()
	srv := fakeEmbed(t, 16)
	t.Cleanup(srv.Close)
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "greet.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n\nfunc Caller() string { return Greet(\"x\") }\n")
	// Sibling test file so the tests lane has something to pair.
	writeFile(t, filepath.Join(projDir, "greet_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) { _ = Greet(\"y\") }\n")
	root = indexProject(t, projDir, cacheDir, srv.URL)
	return newServer(srv.URL, cacheDir), root
}

func TestLocateByRef(t *testing.T) {
	s, root := locateFixture(t)
	// Line 3 is inside Greet's body.
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		Ref: "greet.go:3", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Symbol != "Greet" {
		t.Errorf("symbol = %q, want Greet", out.Symbol)
	}
	if out.Path != "greet.go" {
		t.Errorf("path = %q, want greet.go", out.Path)
	}
	if len(out.Tests) == 0 || out.Tests[0] != "greet_test.go" {
		t.Errorf("tests = %v, want [greet_test.go]", out.Tests)
	}
}

func TestLocateBySymbol(t *testing.T) {
	s, root := locateFixture(t)
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		Symbol: "Greet", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Symbol != "Greet" || out.Path != "greet.go" {
		t.Errorf("got symbol=%q path=%q, want Greet/greet.go", out.Symbol, out.Path)
	}
	// Callers lane is best-effort: when the graph carries calls edges, Caller
	// must be among them; an empty list (no-graph) is tolerated.
	if len(out.Callers) > 0 {
		found := false
		for _, c := range out.Callers {
			if bareSymbolName(c.QualifiedName) == "Caller" {
				found = true
			}
		}
		if !found {
			t.Errorf("callers = %+v, want Caller present", out.Callers)
		}
	}
}

func TestLocateByFrame(t *testing.T) {
	s, root := locateFixture(t)
	// A Go-style frame line carrying a file:location.
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		Frame: "\tgreet.go:3 +0x1a5", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Symbol != "Greet" {
		t.Errorf("symbol = %q, want Greet", out.Symbol)
	}
}

func TestLocateFrameSymbolForm(t *testing.T) {
	s, root := locateFixture(t)
	// A frame with no file:line — only the qualified call expression.
	_, out, err := s.locate(context.Background(), nil, LocateInput{
		Frame: "github.com/x/main.Greet(0xc0001)", ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.Symbol != "Greet" {
		t.Errorf("symbol = %q, want Greet", out.Symbol)
	}
}

func TestLocateNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())
	_, out, _ := s.locate(context.Background(), nil, LocateInput{
		Symbol: "Greet", ProjectRoot: t.TempDir(),
	})
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
	if out.Hint == "" {
		t.Error("expected a hint for no-index")
	}
}

func TestLocateNotFound(t *testing.T) {
	s, root := locateFixture(t)
	cases := []LocateInput{
		{Symbol: "NoSuchSymbol", ProjectRoot: root},
		{Ref: "greet.go:99999", ProjectRoot: root},
		{Ref: "missing.go:1", ProjectRoot: root},
	}
	for _, in := range cases {
		_, out, _ := s.locate(context.Background(), nil, in)
		if out.Status != "not-found" {
			t.Errorf("locate(%+v) status = %q, want not-found", in, out.Status)
		}
	}
}

func TestLocateNoTarget(t *testing.T) {
	s, root := locateFixture(t)
	_, out, _ := s.locate(context.Background(), nil, LocateInput{ProjectRoot: root})
	if out.Status != "error" {
		t.Errorf("status = %q, want error", out.Status)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantLine int
		wantOK   bool
	}{
		{"internal/mcp/server.go:313", "internal/mcp/server.go", 313, true},
		{"foo.go:42:7", "foo.go", 42, true}, // path:line:col
		{"  foo.go:1  ", "foo.go", 1, true},
		{"noline.go", "", 0, false},
		{"foo.go:notanum", "", 0, false},
	}
	for _, c := range cases {
		p, l, ok := parseRef(c.in)
		if ok != c.wantOK || p != c.wantPath || l != c.wantLine {
			t.Errorf("parseRef(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.in, p, l, ok, c.wantPath, c.wantLine, c.wantOK)
		}
	}
}

func TestParseFrame(t *testing.T) {
	cases := []struct {
		in         string
		wantRef    string
		wantSymbol string
	}{
		{"\tinternal/foo.go:42 +0x1a5", "internal/foo.go:42", ""},
		{"panic at server.go:10", "server.go:10", ""},
		{"github.com/x/mcp.(*Server).Run(0xc1)", "", "(*Server).Run"},
		{"github.com/x/mcp.NewServer(0x0)", "", "mcp.NewServer"},
		{"Greet", "", "Greet"},
	}
	for _, c := range cases {
		ref, sym := parseFrame(c.in)
		if ref != c.wantRef || sym != c.wantSymbol {
			t.Errorf("parseFrame(%q) = (ref=%q, sym=%q), want (ref=%q, sym=%q)",
				c.in, ref, sym, c.wantRef, c.wantSymbol)
		}
	}
}

// TestLocateInDefaultSurface guards that locate ships in the everyday tool
// surface (not behind DEX_EXPERT) — it's a headline orientation verb.
func TestLocateInDefaultSurface(t *testing.T) {
	t.Setenv("DEX_EXPERT", "")
	names := listToolNames(t, stubServer(t))
	if !names["locate"] {
		t.Error("default surface omitted verb \"locate\"; want it advertised")
	}
}

// TestLocateSurfacesScopedNote covers #645: a note scoped to a file path is
// surfaced (tagged with scope) when locate touches that file.
func TestLocateSurfacesScopedNote(t *testing.T) {
	s, root := locateFixture(t)
	ctx := context.Background()
	if _, _, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root, Action: "add", Archetype: "Gotcha",
		Body: "greet.go: callers must pre-trim the name", Scope: "greet.go",
	}); err != nil {
		t.Fatal(err)
	}
	_, out, err := s.locate(ctx, nil, LocateInput{Ref: "greet.go:3", ProjectRoot: root})
	if err != nil || out.Status != "ok" {
		t.Fatalf("locate: status=%q hint=%q err=%v", out.Status, out.Hint, err)
	}
	var scoped *LocatedFact
	for i := range out.Notes {
		if out.Notes[i].Scope != "" {
			scoped = &out.Notes[i]
		}
	}
	if scoped == nil {
		t.Fatal("expected a scope-matched note surfaced on touch (#645)")
	}
	if scoped.Scope != "greet.go" || !strings.Contains(scoped.Body, "pre-trim") {
		t.Errorf("wrong scoped note: %+v", scoped)
	}
}
