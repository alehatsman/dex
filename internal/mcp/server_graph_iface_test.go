package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// seedReaderFixture builds an indexed graph: interface Reader{Read}, *File
// implementing it, useFile (static call to (*File).Read) and useReader (call to
// (Reader).Read — dynamic dispatch). Returns the project dir + cache dir.
func seedReaderFixture(t *testing.T) (projDir, cacheDir string) {
	t.Helper()
	projDir = t.TempDir()
	cacheDir = t.TempDir()
	writeFile(t, filepath.Join(projDir, "p.go"), "package p\n")
	p, err := proj.Resolve(projDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	now := time.Now()
	nodes := []store.GraphNodeRow{
		{ID: "t_reader", Kind: string(graph.NodeType), Name: "Reader", QualifiedName: "Reader", PackagePath: "p", FilePath: "p/r.go", StartLine: 1, EndLine: 3, MetadataJSON: []byte("{}"), ContentHash: "1"},
		{ID: "m_reader_read", Kind: string(graph.NodeMethod), Name: "Read", QualifiedName: "(Reader).Read", PackagePath: "p", FilePath: "p/r.go", StartLine: 2, EndLine: 2, MetadataJSON: []byte("{}"), ContentHash: "2"},
		{ID: "t_file", Kind: string(graph.NodeType), Name: "File", QualifiedName: "File", PackagePath: "p", FilePath: "p/f.go", StartLine: 1, EndLine: 5, MetadataJSON: []byte("{}"), ContentHash: "3"},
		{ID: "m_file_read", Kind: string(graph.NodeMethod), Name: "Read", QualifiedName: "(*File).Read", PackagePath: "p", FilePath: "p/f.go", StartLine: 10, EndLine: 15, MetadataJSON: []byte("{}"), ContentHash: "4"},
		{ID: "f_usefile", Kind: string(graph.NodeFunction), Name: "useFile", QualifiedName: "useFile", PackagePath: "p", FilePath: "p/use.go", StartLine: 20, EndLine: 22, MetadataJSON: []byte("{}"), ContentHash: "5"},
		{ID: "f_usereader", Kind: string(graph.NodeFunction), Name: "useReader", QualifiedName: "useReader", PackagePath: "p", FilePath: "p/use.go", StartLine: 30, EndLine: 32, MetadataJSON: []byte("{}"), ContentHash: "6"},
	}
	if err := st.GraphUpsertNodes(context.Background(), nodes, now); err != nil {
		t.Fatal(err)
	}
	edges := []store.GraphEdgeRow{
		{ID: "e_hm", Kind: string(graph.EdgeHasMethod), SrcID: "t_file", DstID: "m_file_read", FilePath: "p/f.go", StartLine: 10, EndLine: 15, MetadataJSON: []byte("{}"), ContentHash: "h1"},
		{ID: "e_impl", Kind: string(graph.EdgeImplements), SrcID: "t_file", DstID: "t_reader", FilePath: "p/f.go", StartLine: 1, EndLine: 1, MetadataJSON: []byte("{}"), ContentHash: "h2"},
		{ID: "e_static", Kind: string(graph.EdgeCalls), SrcID: "f_usefile", DstID: "m_file_read", FilePath: "p/use.go", StartLine: 21, EndLine: 21, MetadataJSON: []byte("{}"), ContentHash: "h3"},
		{ID: "e_disp", Kind: string(graph.EdgeCalls), SrcID: "f_usereader", DstID: "m_reader_read", FilePath: "p/use.go", StartLine: 31, EndLine: 31, MetadataJSON: []byte("{}"), ContentHash: "h4"},
	}
	if err := st.GraphUpsertEdges(context.Background(), edges, now); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return projDir, cacheDir
}

// TestTraceInterfaceDispatchCallers covers #604: tracing callers of a concrete
// method that implements a project interface must ALSO surface the callers of
// the interface method (which dispatch to it), tagged with Via.
func TestTraceInterfaceDispatchCallers(t *testing.T) {
	projDir, cacheDir := seedReaderFixture(t)
	s := &Server{IndexDir: cacheDir}
	out, err := s.GraphCallers(context.Background(), CallEdgeInput{Name: "(*File).Read", ProjectRoot: projDir})
	if err != nil || out.Status != "ok" {
		t.Fatalf("GraphCallers: status=%q hint=%q err=%v", out.Status, out.Hint, err)
	}

	var static, dispatch *CallSite
	for i := range out.Hits {
		switch out.Hits[i].QualifiedName {
		case "useFile":
			static = &out.Hits[i]
		case "useReader":
			dispatch = &out.Hits[i]
		}
	}
	if static == nil {
		t.Error("expected the direct static caller useFile")
	} else if static.Via != "" {
		t.Errorf("static caller should have empty Via, got %q", static.Via)
	}
	if dispatch == nil {
		t.Fatal("expected the interface-dispatch caller useReader (the #604 fix)")
	}
	if dispatch.Via != "(Reader).Read" {
		t.Errorf("dispatch caller Via = %q, want (Reader).Read", dispatch.Via)
	}
}

// TestTraceNoFalseDispatchForUnrelatedMethod guards the name match: a type
// implementing an interface must not pick up dispatch callers for a method the
// interface does NOT declare.
func TestTraceNoFalseDispatchForUnrelatedMethod(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "p.go"), "package p\n")

	p, _ := proj.Resolve(projDir, cacheDir)
	if err := p.EnsureCacheDir(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), p.DBPath)
	if err != nil {
		t.Skip("fts5 not available:", err)
	}
	now := time.Now()
	// Reader declares only Read; File also has Close (NOT on the interface).
	nodes := []store.GraphNodeRow{
		{ID: "t_reader", Kind: string(graph.NodeType), Name: "Reader", QualifiedName: "Reader", PackagePath: "p", FilePath: "p/r.go", MetadataJSON: []byte("{}"), ContentHash: "1"},
		{ID: "m_reader_read", Kind: string(graph.NodeMethod), Name: "Read", QualifiedName: "(Reader).Read", PackagePath: "p", FilePath: "p/r.go", MetadataJSON: []byte("{}"), ContentHash: "2"},
		{ID: "t_file", Kind: string(graph.NodeType), Name: "File", QualifiedName: "File", PackagePath: "p", FilePath: "p/f.go", MetadataJSON: []byte("{}"), ContentHash: "3"},
		{ID: "m_file_close", Kind: string(graph.NodeMethod), Name: "Close", QualifiedName: "(*File).Close", PackagePath: "p", FilePath: "p/f.go", StartLine: 10, EndLine: 12, MetadataJSON: []byte("{}"), ContentHash: "4"},
		{ID: "f_caller", Kind: string(graph.NodeFunction), Name: "callRead", QualifiedName: "callRead", PackagePath: "p", FilePath: "p/u.go", StartLine: 20, EndLine: 22, MetadataJSON: []byte("{}"), ContentHash: "5"},
	}
	_ = st.GraphUpsertNodes(context.Background(), nodes, now)
	edges := []store.GraphEdgeRow{
		{ID: "e_hm", Kind: string(graph.EdgeHasMethod), SrcID: "t_file", DstID: "m_file_close", MetadataJSON: []byte("{}"), ContentHash: "h1"},
		{ID: "e_impl", Kind: string(graph.EdgeImplements), SrcID: "t_file", DstID: "t_reader", MetadataJSON: []byte("{}"), ContentHash: "h2"},
		// caller invokes (Reader).Read — must NOT be attributed to (*File).Close.
		{ID: "e_disp", Kind: string(graph.EdgeCalls), SrcID: "f_caller", DstID: "m_reader_read", FilePath: "p/u.go", StartLine: 21, MetadataJSON: []byte("{}"), ContentHash: "h3"},
	}
	_ = st.GraphUpsertEdges(context.Background(), edges, now)
	st.Close()

	s := &Server{IndexDir: cacheDir}
	out, _ := s.GraphCallers(context.Background(), CallEdgeInput{Name: "(*File).Close", ProjectRoot: projDir})
	for _, h := range out.Hits {
		if h.QualifiedName == "callRead" {
			t.Errorf("Close must NOT pick up Read's interface callers (name mismatch), got %+v", h)
		}
	}
}

// TestImpactInterfaceDispatch covers the #604 impact follow-up: the blast
// radius (and risk) of a method must include callers that reach it through an
// interface, not only static callers.
func TestImpactInterfaceDispatch(t *testing.T) {
	projDir, cacheDir := seedReaderFixture(t)
	s := &Server{IndexDir: cacheDir}

	out, err := s.GraphImpact(context.Background(), ImpactInput{Name: "(*File).Read", ProjectRoot: projDir})
	if err != nil || out.Status != "ok" {
		t.Fatalf("GraphImpact: status=%q hint=%q err=%v", out.Status, out.Hint, err)
	}
	var hasStatic, hasDispatch bool
	for _, n := range out.Nodes {
		switch n.QualifiedName {
		case "useFile":
			hasStatic = true
		case "useReader":
			hasDispatch = true
		}
	}
	if !hasStatic {
		t.Error("impact should include the static caller useFile")
	}
	if !hasDispatch {
		t.Error("impact must include the interface-dispatch caller useReader (#604 impact enrichment)")
	}
}
