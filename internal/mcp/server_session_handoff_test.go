package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// handoffProject indexes a temp project with one real file on disk (so export
// can compute a content etag) and returns its root.
func handoffProject(t *testing.T, srvURL string) (projDir, cacheDir, root string) {
	t.Helper()
	cacheDir = t.TempDir()
	projDir = t.TempDir()
	writeFile(t, projDir+"/svc.go", "package svc\n\nfunc Handle() {}\n")
	root = indexProject(t, projDir, cacheDir, srvURL)
	return projDir, cacheDir, root
}

// seedHandoffSession declares a task, tracks the file, and adds a note via the
// session tool — the working set an export should capture.
func seedHandoffSession(t *testing.T, s *Server, root string) {
	t.Helper()
	ctx := context.Background()
	for _, in := range []SessionInput{
		{Action: "set_task", Task: "wire the dispatcher", ProjectRoot: root},
		{Action: "add_file", File: "svc.go", Op: "read", ProjectRoot: root},
		{Action: "add_note", Note: "Handle is the entrypoint", ProjectRoot: root},
	} {
		if _, out, err := s.session(ctx, nil, in); err != nil || out.Status != "ok" {
			t.Fatalf("seed %s: status=%q err=%v hint=%s", in.Action, out.Status, err, out.Hint)
		}
	}
}

// TestSessionExportImportRoundTrip: export serialises task + file (with etag) +
// notes into a dex-session-v1 bundle; import into a cleared session restores the
// state and returns a recovery digest.
func TestSessionExportImportRoundTrip(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := handoffProject(t, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	seedHandoffSession(t, s, root)

	// Export.
	_, exp, err := s.session(context.Background(), nil, SessionInput{Action: "export", ProjectRoot: root})
	if err != nil || exp.Status != "ok" {
		t.Fatalf("export: status=%q err=%v hint=%s", exp.Status, err, exp.Hint)
	}
	var b sessionBundle
	if err := json.Unmarshal([]byte(exp.Content), &b); err != nil {
		t.Fatalf("bundle is not valid JSON: %v\n%s", err, exp.Content)
	}
	if b.Schema != sessionBundleSchema {
		t.Errorf("schema = %q, want %q", b.Schema, sessionBundleSchema)
	}
	if b.Task != "wire the dispatcher" {
		t.Errorf("task = %q", b.Task)
	}
	if !strings.Contains(b.Notes, "Handle is the entrypoint") {
		t.Errorf("notes missing seeded note: %q", b.Notes)
	}
	if len(b.Files) != 1 || b.Files[0].Path != "svc.go" || b.Files[0].Etag == "" {
		t.Fatalf("files = %+v, want one svc.go with an etag", b.Files)
	}
	// Privacy: the bundle must not carry file content.
	if strings.Contains(exp.Content, "func Handle") {
		t.Errorf("bundle leaked file content:\n%s", exp.Content)
	}

	// Wipe the session, then import the bundle.
	if _, out, err := s.session(context.Background(), nil, SessionInput{Action: "clear", ProjectRoot: root}); err != nil || out.Status != "ok" {
		t.Fatalf("clear: status=%q err=%v", out.Status, err)
	}
	_, imp, err := s.session(context.Background(), nil, SessionInput{Action: "import", Bundle: exp.Content, ProjectRoot: root})
	if err != nil || imp.Status != "ok" {
		t.Fatalf("import: status=%q err=%v hint=%s", imp.Status, err, imp.Hint)
	}
	if imp.Task != "wire the dispatcher" {
		t.Errorf("import Task = %q", imp.Task)
	}
	for _, want := range []string{"Session Handoff Imported", "svc.go", "wire the dispatcher"} {
		if !strings.Contains(imp.Content, want) {
			t.Errorf("import digest missing %q\n%s", want, imp.Content)
		}
	}
	if strings.Contains(imp.Content, "Changed since handoff") {
		t.Errorf("unmodified file should not be flagged stale\n%s", imp.Content)
	}

	// State is actually restored: a get returns the task + file.
	_, got, err := s.session(context.Background(), nil, SessionInput{Action: "get", ProjectRoot: root})
	if err != nil || got.Status != "ok" {
		t.Fatalf("get: status=%q err=%v", got.Status, err)
	}
	if got.Task != "wire the dispatcher" || got.FileCount != 1 {
		t.Errorf("restored session: task=%q files=%d, want task set + 1 file", got.Task, got.FileCount)
	}
}

// TestSessionImportFlagsStaleFile: a file whose content changed since export is
// called out in the import digest so the agent re-reads it first.
func TestSessionImportFlagsStaleFile(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	projDir, cacheDir, root := handoffProject(t, srv.URL)

	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()
	seedHandoffSession(t, s, root)

	_, exp, err := s.session(context.Background(), nil, SessionInput{Action: "export", ProjectRoot: root})
	if err != nil || exp.Status != "ok" {
		t.Fatalf("export: status=%q err=%v", exp.Status, err)
	}

	// Mutate the file on disk so its etag no longer matches the bundle.
	writeFile(t, projDir+"/svc.go", "package svc\n\nfunc Handle() { println(\"changed\") }\n")

	_, imp, err := s.session(context.Background(), nil, SessionInput{Action: "import", Bundle: exp.Content, ProjectRoot: root})
	if err != nil || imp.Status != "ok" {
		t.Fatalf("import: status=%q err=%v", imp.Status, err)
	}
	if !strings.Contains(imp.Content, "Changed since handoff") || !strings.Contains(imp.Content, "svc.go") {
		t.Errorf("changed file not flagged stale\n%s", imp.Content)
	}
}

// TestSessionImportRejectsBadInput: an empty or wrong-schema bundle fails closed.
func TestSessionImportRejectsBadInput(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := handoffProject(t, srv.URL)
	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()

	cases := []struct {
		name, bundle, wantHint string
	}{
		{"empty", "", "requires the exported bundle"},
		{"bad-json", "{not json", "parse bundle"},
		{"wrong-schema", `{"schema":"dex-session-v99"}`, "unsupported bundle schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, out, err := s.session(context.Background(), nil, SessionInput{Action: "import", Bundle: c.bundle, ProjectRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "error" || !strings.Contains(out.Hint, c.wantHint) {
				t.Errorf("status=%q hint=%q, want error containing %q", out.Status, out.Hint, c.wantHint)
			}
		})
	}
}

// TestSessionExportNoSession: export with no active session is a clean no-op.
func TestSessionExportNoSession(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	_, cacheDir, root := handoffProject(t, srv.URL)
	s := newServer(srv.URL, cacheDir)
	defer s.waitSessionWrites()

	_, out, err := s.session(context.Background(), nil, SessionInput{Action: "export", ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" || !strings.Contains(out.Hint, "no active session") {
		t.Errorf("status=%q hint=%q, want ok + no-active-session note", out.Status, out.Hint)
	}
}
