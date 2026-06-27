package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestKnowledgeImportDirectJSON verifies the import action accepts a raw JSON
// array in the body param (the normal CLI path).
func TestKnowledgeImportDirectJSON(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	rows := []knowledgeExportRow{
		{Archetype: "Observation", Body: "direct json import fact", Confidence: 0.8},
	}
	body, _ := json.Marshal(rows)

	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root,
		Action:      "import",
		Body:        string(body),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%q, want ok", out.Status, out.Hint)
	}

	// Verify the fact was stored.
	_, list, err := s.knowledge(ctx, nil, KnowledgeInput{ProjectRoot: root, Action: "list", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range list.Facts {
		if f.Body == "direct json import fact" {
			found = true
			break
		}
	}
	if !found {
		t.Error("imported fact not found in list")
	}
}

// TestKnowledgeImportJSONStringifiedBody verifies the import action unwraps a
// JSON-stringified body param — the MCP content-envelope pattern where the
// array is marshalled into a string before being placed in the tool param
// (#748, same double-encode as #734 parseAskResponse).
func TestKnowledgeImportJSONStringifiedBody(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	rows := []knowledgeExportRow{
		{Archetype: "Gotcha", Body: "mcp envelope import fact", Confidence: 0.9},
	}
	inner, _ := json.Marshal(rows)

	// Simulate MCP double-encode: the JSON array is itself JSON-marshalled into a
	// string (so the body param value is a quoted JSON string, not a bare array).
	outer, _ := json.Marshal(string(inner))

	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root,
		Action:      "import",
		Body:        string(outer),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "ok" {
		t.Fatalf("status=%q hint=%q, want ok (body was double-encoded)", out.Status, out.Hint)
	}

	// Verify the fact was stored.
	_, list, err := s.knowledge(ctx, nil, KnowledgeInput{ProjectRoot: root, Action: "list", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range list.Facts {
		if f.Body == "mcp envelope import fact" {
			found = true
			break
		}
	}
	if !found {
		t.Error("imported fact not found in list after double-encoded body unwrap")
	}
}

// TestKnowledgeImportInvalidBody verifies that a malformed body returns an
// error status, not a panic or silent no-op.
func TestKnowledgeImportInvalidBody(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, out, err := s.knowledge(ctx, nil, KnowledgeInput{
		ProjectRoot: root,
		Action:      "import",
		Body:        "not-valid-json-at-all",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "error" {
		t.Fatalf("status=%q, want error for invalid body", out.Status)
	}
}
