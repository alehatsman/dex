package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
)

// chatDownFixture indexes a small project with a WORKING embed backend (so the
// ask retrieval lanes and file_view map mode work), then returns the resolved
// project root and the cache dir. The caller wires whatever ChatClient the case
// needs (nil, unreachable, or healthy).
func chatDownFixture(t *testing.T) (projRoot, cacheDir string) {
	t.Helper()
	embedSrv := fakeEmbed(t, 16)
	t.Cleanup(embedSrv.Close)

	projDir := t.TempDir()
	writeFile(t, projDir+"/auth.go", "package x\n\n// Authenticate validates a bearer token and returns an error when invalid.\nfunc Authenticate(token string) error {\n\tif token == \"\" {\n\t\treturn errInvalid\n\t}\n\treturn nil\n}\n\nvar errInvalid = errorString(\"invalid token\")\n\ntype errorString string\n\nfunc (e errorString) Error() string { return string(e) }\n")
	cacheDir = t.TempDir()
	projRoot = indexProject(t, projDir, cacheDir, embedSrv.URL)
	return projRoot, cacheDir
}

func chatDownServer(t *testing.T, cacheDir string, chatClient chat.Chatter) *Server {
	t.Helper()
	embedSrv := fakeEmbed(t, 16)
	t.Cleanup(embedSrv.Close)
	s := newServer(embedSrv.URL, cacheDir)
	s.ChatClient = chatClient
	return s
}

// TestAskDegradesWhenChatDown locks the ask chat-leg contract (issue #176):
// when the chat model is nil or unreachable, ask returns an evidence-only
// bundle with status "ok" and an empty Answer — synthesis failure never breaks
// ask. The healthy-chat subtest is the control: it proves the same question +
// evidence DOES yield an Answer when chat is up, so the degraded assertions
// aren't vacuously satisfied.
func TestAskDegradesWhenChatDown(t *testing.T) {
	projRoot, cacheDir := chatDownFixture(t)
	ctx := context.Background()
	const question = "what does Authenticate do"

	assertEvidence := func(t *testing.T, out ContextOutput) {
		t.Helper()
		if out.Status != "ok" {
			t.Fatalf("status = %q, want ok (degraded chat must not change status)", out.Status)
		}
		if len(out.Symbols) == 0 && len(out.SemanticHits) == 0 && len(out.SuggestedReads) == 0 {
			t.Error("evidence bundle is empty; ask should still return retrieval evidence with chat down")
		}
	}

	t.Run("nil chat client", func(t *testing.T) {
		resetAnswerCache(t)
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, Project: projRoot})
		if err != nil {
			t.Fatalf("ask returned hard error with nil chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (no chat client wired)", out.Answer)
		}
	})

	t.Run("unreachable chat client", func(t *testing.T) {
		resetAnswerCache(t)
		// Closed port → chat.ErrUnreachable from Generate.
		s := chatDownServer(t, cacheDir, chat.New(closedURL(t), "fake", 200*time.Millisecond))
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, Project: projRoot})
		if err != nil {
			t.Fatalf("ask returned hard error with unreachable chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (chat unreachable)", out.Answer)
		}
	})

	t.Run("healthy chat control", func(t *testing.T) {
		resetAnswerCache(t)
		chatSrv := fakeChat(t, "Authenticate validates a bearer token (auth.go).")
		defer chatSrv.Close()
		s := chatDownServer(t, cacheDir, chat.New(chatSrv.URL, "fake", 5*time.Second))
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, Project: projRoot})
		if err != nil {
			t.Fatalf("ask errored with healthy chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer == "" {
			t.Fatal("Answer is empty with a healthy chat client; degraded subtests would be vacuous")
		}
	})
}

// TestFileViewFullDegradesWhenChatDown locks the file_view "full" chat-leg
// contract: a nil chat client downgrades full → map (no LLM), and an
// unreachable chat client returns the raw file content with status "ok" and an
// "offline" hint — never a crash or hard error.
func TestFileViewFullDegradesWhenChatDown(t *testing.T) {
	projRoot, cacheDir := chatDownFixture(t)
	ctx := context.Background()

	t.Run("nil chat downgrades full to map", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "auth.go", Mode: "full", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("file_view full returned hard error with nil chat: %v", err)
		}
		if out.Status != "ok" {
			t.Errorf("status = %q, want ok (full must downgrade to map, not crash)", out.Status)
		}
		// The LLM path was never taken: Endpoint/Model are only populated
		// when full mode actually runs against the chat client. With no chat
		// client wired, full silently downgrades and these stay empty.
		if out.Model != "" || out.Endpoint != "" {
			t.Errorf("chat metadata set (model=%q endpoint=%q); full must NOT engage chat when none is wired", out.Model, out.Endpoint)
		}
		// A well-formed degraded response: either map content or an
		// actionable hint, not a silent empty body with no explanation.
		if strings.TrimSpace(out.Content) == "" && strings.TrimSpace(out.Hint) == "" {
			t.Error("downgraded file_view returned neither content nor hint")
		}
	})

	t.Run("unreachable chat shows raw content", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, chat.New(closedURL(t), "fake", 200*time.Millisecond))
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "auth.go", Mode: "full", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("file_view full returned hard error with unreachable chat: %v", err)
		}
		if out.Status != "ok" {
			t.Errorf("status = %q, want ok (unreachable chat must degrade to raw content)", out.Status)
		}
		if !strings.Contains(out.Content, "func Authenticate") {
			t.Errorf("expected raw file content on chat outage, got %q", out.Content)
		}
		if !strings.Contains(strings.ToLower(out.Hint), "offline") {
			t.Errorf("hint = %q, want an 'offline' degradation note", out.Hint)
		}
	})
}
