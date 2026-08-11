package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The act/remember verbs are thin envelope facades over shell/knowledge (#110).
// These tests pin the envelope shape and the compose-over-existing-handler
// behavior; the underlying exec and store are covered by their own suites.

func TestActVerbEnvelope(t *testing.T) {
	s := stubServer(t)
	_, out, err := actVerb(context.Background(), s, &sdk.CallToolRequest{}, ActInput{Command: "echo hello-act"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint=%q)", out.Status, out.Hint)
	}
	if out.Trust.Provenance != "exact" {
		t.Errorf("trust.provenance = %q, want exact", out.Trust.Provenance)
	}
	if out.Result.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.Result.ExitCode)
	}
	if !strings.Contains(out.Result.Output, "hello-act") {
		t.Errorf("result.output missing command output: %q", out.Result.Output)
	}
}

func TestActVerbEmptyCommandErrors(t *testing.T) {
	s := stubServer(t)
	_, out, _ := actVerb(context.Background(), s, &sdk.CallToolRequest{}, ActInput{Command: "   "})
	if out.Status != "error" {
		t.Errorf("empty command: status = %q, want error", out.Status)
	}
	if out.Trust.Provenance != "exact" {
		t.Errorf("trust.provenance = %q, want exact even on the guard path", out.Trust.Provenance)
	}
}

func TestActVerbPropagatesExitCode(t *testing.T) {
	s := stubServer(t)
	_, out, err := actVerb(context.Background(), s, &sdk.CallToolRequest{}, ActInput{Command: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", out.Result.ExitCode)
	}
}

func TestRememberWriteThenRecall(t *testing.T) {
	s := stubServer(t)
	ctx := context.Background()
	root := t.TempDir()

	_, w, err := rememberVerb(ctx, s, &sdk.CallToolRequest{},
		RememberInput{Fact: "[Decision] merges are FF-only on this repo", ProjectRoot: root})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if w.Result.Mode != "wrote" {
		t.Errorf("write mode = %q, want wrote (status=%q hint=%q)", w.Result.Mode, w.Status, w.Hint)
	}
	if w.Trust.Provenance != "exact" {
		t.Errorf("write trust.provenance = %q, want exact", w.Trust.Provenance)
	}

	_, r, err := rememberVerb(ctx, s, &sdk.CallToolRequest{},
		RememberInput{Query: "merge policy", ProjectRoot: root})
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

// TestRememberSupersede pins the 5d fold (#147): remember absorbs notes'
// supersede move — a write with Supersedes=<id> marks the old fact inactive in
// one call (no separate notes tool), so recall no longer returns the stale fact.
// Uses the fakeEmbed+indexProject harness so add→recall genuinely round-trips
// (a query equal to a fact body yields cosine 1.0).
func TestRememberSupersede(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\n\nfunc main() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)
	s := newServer(srv.URL, cacheDir)
	ctx := context.Background()

	_, w0, err := rememberVerb(ctx, s, nil,
		RememberInput{ProjectRoot: root, Archetype: "Convention", Fact: "indent with tabs"})
	if err != nil || w0.Result.Mode != "wrote" {
		t.Fatalf("write stale: mode=%q status=%q err=%v", w0.Result.Mode, w0.Status, err)
	}
	_, r0, err := rememberVerb(ctx, s, nil,
		RememberInput{ProjectRoot: root, Query: "indent with tabs", K: 5})
	if err != nil || len(r0.Result.Facts) == 0 {
		t.Fatalf("recall stale: err=%v facts=%d", err, len(r0.Result.Facts))
	}
	staleID := r0.Result.Facts[0].ID

	_, w1, err := rememberVerb(ctx, s, nil,
		RememberInput{ProjectRoot: root, Archetype: "Convention", Fact: "indent with spaces", Supersedes: staleID})
	if err != nil || w1.Result.Mode != "wrote" {
		t.Fatalf("supersede: mode=%q status=%q err=%v", w1.Result.Mode, w1.Status, err)
	}

	_, r1, err := rememberVerb(ctx, s, nil,
		RememberInput{ProjectRoot: root, Query: "indent with tabs", K: 5})
	if err != nil {
		t.Fatalf("recall after supersede: %v", err)
	}
	for _, f := range r1.Result.Facts {
		if f.ID == staleID {
			t.Errorf("superseded fact id=%d still recalled (body=%q); supersedes not wired through", staleID, f.Body)
		}
	}
}
