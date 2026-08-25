package mcp

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The record verb is a thin envelope facade over the knowledge store (#110).
// These tests pin the envelope shape and the compose-over-existing-handler
// behavior; the underlying store is covered by its own suite.

func TestRecordWriteThenRecall(t *testing.T) {
	s := stubServer(t)
	ctx := context.Background()
	root := t.TempDir()

	_, w, err := recordVerb(ctx, s, &sdk.CallToolRequest{},
		RecordInput{Fact: "[Decision] merges are FF-only on this repo", ProjectRoot: root})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if w.Result.Mode != "wrote" {
		t.Errorf("write mode = %q, want wrote (status=%q hint=%q)", w.Result.Mode, w.Status, w.Hint)
	}
	if w.Trust.Provenance != "exact" {
		t.Errorf("write trust.provenance = %q, want exact", w.Trust.Provenance)
	}

	_, r, err := recordVerb(ctx, s, &sdk.CallToolRequest{},
		RecordInput{Query: "merge policy", ProjectRoot: root})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if r.Result.Mode != "recalled" {
		t.Errorf("recall mode = %q, want recalled", r.Result.Mode)
	}
	if r.Trust.Provenance != "exact" {
		t.Errorf("recall trust.provenance = %q, want exact", r.Trust.Provenance)
	}
}

// TestRecordSupersede pins the 5d fold (#147): record absorbs notes'
// supersede move — a write with Supersedes=<id> marks the old fact inactive in
// one call (no separate notes tool), so recall no longer returns the stale fact.
// Uses the fakeEmbed+indexProject harness so add→recall genuinely round-trips
// (a query equal to a fact body yields cosine 1.0).
func TestRecordSupersede(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, w0, err := recordVerb(ctx, s, nil,
		RecordInput{ProjectRoot: root, Archetype: "Convention", Fact: "indent with tabs"})
	if err != nil || w0.Result.Mode != "wrote" {
		t.Fatalf("write stale: mode=%q status=%q err=%v", w0.Result.Mode, w0.Status, err)
	}
	_, r0, err := recordVerb(ctx, s, nil,
		RecordInput{ProjectRoot: root, Query: "indent with tabs", K: 5})
	if err != nil || len(r0.Result.Facts) == 0 {
		t.Fatalf("recall stale: err=%v facts=%d", err, len(r0.Result.Facts))
	}
	staleID := r0.Result.Facts[0].ID

	_, w1, err := recordVerb(ctx, s, nil,
		RecordInput{ProjectRoot: root, Archetype: "Convention", Fact: "indent with spaces", Supersedes: staleID})
	if err != nil || w1.Result.Mode != "wrote" {
		t.Fatalf("supersede: mode=%q status=%q err=%v", w1.Result.Mode, w1.Status, err)
	}

	_, r1, err := recordVerb(ctx, s, nil,
		RecordInput{ProjectRoot: root, Query: "indent with tabs", K: 5})
	if err != nil {
		t.Fatalf("recall after supersede: %v", err)
	}
	for _, f := range r1.Result.Facts {
		if f.ID == staleID {
			t.Errorf("superseded fact id=%d still recalled (body=%q); supersedes not wired through", staleID, f.Body)
		}
	}
}
