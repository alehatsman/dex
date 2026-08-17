package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// TestPeerClaimCaveat: a peer's fresh claim on a file surfaces as a caveat on
// look; the asking agent's own claim does not.
func TestPeerClaimCaveat(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\nfunc F() {}\n")
	root, st := openIndexedStore(t, srv.URL, cacheDir, projDir)

	post := func(agent, file, intent string) {
		if _, err := st.AgentPost(context.Background(), agent, store.NormalizeClaimPath(file), "claim", intent); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	post("alice", "main.go", "renaming F")
	post("me", "main.go", "also me") // our own claim — must not surface to us

	s := newServer(srv.URL, cacheDir)
	s.AgentID = "me"

	caveat, ok := s.peerClaimCaveat(context.Background(), root, "main.go")
	if !ok {
		t.Fatalf("expected a peer claim caveat, got none")
	}
	if !strings.Contains(caveat, "alice") || !strings.Contains(caveat, "renaming F") {
		t.Errorf("caveat missing peer/intent: %q", caveat)
	}
	if strings.Contains(caveat, "peers are editing") {
		t.Errorf("self-claim was counted: %q", caveat)
	}

	// A file no peer claims yields nothing.
	if _, ok := s.peerClaimCaveat(context.Background(), root, "nonexistent.go"); ok {
		t.Errorf("unexpected caveat for unclaimed file")
	}
}

// TestFlagPeerClaimsAppends: flagPeerClaims appends to an existing caveat
// rather than clobbering it (both signals can hold).
func TestFlagPeerClaimsAppends(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()
	writeFile(t, projDir+"/main.go", "package main\nfunc F() {}\n")
	root, st := openIndexedStore(t, srv.URL, cacheDir, projDir)
	if _, err := st.AgentPost(context.Background(), "alice", "main.go", "claim", "editing"); err != nil {
		t.Fatal(err)
	}

	s := newServer(srv.URL, cacheDir)
	s.AgentID = "me"
	out := LookOutput{Trust: EnvTrust{Caveat: "index rebuilding"}}
	flagPeerClaims(context.Background(), s, root, "main.go", &out)

	if !strings.HasPrefix(out.Trust.Caveat, "index rebuilding") {
		t.Errorf("existing caveat clobbered: %q", out.Trust.Caveat)
	}
	if !strings.Contains(out.Trust.Caveat, "alice") {
		t.Errorf("claim caveat not appended: %q", out.Trust.Caveat)
	}
}
