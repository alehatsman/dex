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
	// A wired client stands in for a real, selected model: mark it configured
	// so the synthesis gate (#160) treats it as usable. Cases that need the
	// unconfigured-default path (client wired, no model) override this to false.
	s.ChatConfigured = chatClient != nil
	return s
}

// TestAskDegradesWhenChatDown locks the ask chat-leg contract (issue #176):
// when the chat model is nil or unreachable, ask returns an evidence-only
// bundle with status "ok" and an empty Answer — synthesis failure never breaks
// ask. The healthy-chat subtest is the control: it proves the same question +
// evidence DOES yield an Answer when chat is up and answer_style:"brief" is
// set, so the degraded assertions aren't vacuously satisfied.
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
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("ask returned hard error with nil chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (no chat client wired)", out.Answer)
		}
	})

	t.Run("unreachable chat client", func(t *testing.T) {
		// Closed port → chat.ErrUnreachable from Generate.
		s := chatDownServer(t, cacheDir, chat.New(closedURL(t), "fake", 200*time.Millisecond))
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("ask returned hard error with unreachable chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (chat unreachable)", out.Answer)
		}
	})

	t.Run("healthy chat with answer_style none (default) skips synthesis", func(t *testing.T) {
		chatSrv := fakeChat(t, "Authenticate validates a bearer token (auth.go).")
		defer chatSrv.Close()
		s := chatDownServer(t, cacheDir, chat.New(chatSrv.URL, "fake", 5*time.Second))
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("ask errored with healthy chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (answer_style defaults to none)", out.Answer)
		}
	})

	t.Run("healthy chat with answer_style brief synthesizes", func(t *testing.T) {
		chatSrv := fakeChat(t, "Authenticate validates a bearer token (auth.go).")
		defer chatSrv.Close()
		s := chatDownServer(t, cacheDir, chat.New(chatSrv.URL, "fake", 5*time.Second))
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, ProjectRoot: projRoot, AnswerStyle: "brief"})
		if err != nil {
			t.Fatalf("ask errored with healthy chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer == "" {
			t.Fatal("Answer is empty with answer_style:brief and healthy chat")
		}
	})

	// #160: a chat client is wired but no model was actually selected (bare
	// DEX_CHAT_URL, empty DEX_CHAT_MODEL → the client carries a fabricated
	// default the endpoint won't serve). answer_style:brief must NOT fire the
	// call — doing so leaks an upstream 404 into the hint. The endpoint here is
	// HEALTHY on purpose: it proves the ChatConfigured gate, not a broken chat,
	// is what suppresses synthesis, and that the hint steers to DEX_CHAT_MODEL.
	t.Run("chat wired but not configured skips synthesis with hint", func(t *testing.T) {
		chatSrv := fakeChat(t, "Authenticate validates a bearer token (auth.go).")
		defer chatSrv.Close()
		s := chatDownServer(t, cacheDir, chat.New(chatSrv.URL, "fake", 5*time.Second))
		s.ChatConfigured = false // model was never selected; default is fabricated
		_, out, err := s.contextRouter(ctx, nil, ContextInput{Question: question, ProjectRoot: projRoot, AnswerStyle: "brief"})
		if err != nil {
			t.Fatalf("ask errored with unconfigured chat: %v", err)
		}
		assertEvidence(t, out)
		if out.Answer != "" {
			t.Errorf("Answer = %q, want empty (synthesis must be gated when chat is not configured)", out.Answer)
		}
		if !strings.Contains(strings.ToLower(out.Hint), "not configured") {
			t.Errorf("hint = %q, want a 'not configured' note pointing at DEX_CHAT_MODEL", out.Hint)
		}
	})
}

// TestReadModesAndChatDegradation locks the read contract after the v1 mode
// flip (#427): `full` is raw content needing no chat, and `summary` is the only
// LLM path — it returns status "needs-chat" when no chat client is wired, and
// degrades to raw content with an "offline" hint when the chat client is
// unreachable. Never a crash or hard error.
func TestReadModesAndChatDegradation(t *testing.T) {
	projRoot, cacheDir := chatDownFixture(t)
	ctx := context.Background()

	t.Run("full is raw content, no chat engaged", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "auth.go", Mode: "full", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("read full returned hard error with nil chat: %v", err)
		}
		if out.Status != "ok" {
			t.Errorf("status = %q, want ok (full is raw, needs no chat)", out.Status)
		}
		if !strings.Contains(out.Content, "func Authenticate") {
			t.Errorf("expected raw file content for mode=full, got %q", out.Content)
		}
		// The LLM path was never taken: chat metadata stays empty for raw modes.
		if out.Model != "" || out.Endpoint != "" {
			t.Errorf("chat metadata set (model=%q endpoint=%q); full must NOT engage chat", out.Model, out.Endpoint)
		}
	})

	t.Run("summary with no chat returns needs-chat", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, nil)
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "auth.go", Mode: "summary", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("read summary returned hard error with nil chat: %v", err)
		}
		if out.Status != "needs-chat" {
			t.Errorf("status = %q, want needs-chat (summary requires a chat model)", out.Status)
		}
		if strings.TrimSpace(out.Hint) == "" {
			t.Error("needs-chat response should carry an actionable hint")
		}
	})

	t.Run("summary with unreachable chat degrades to raw content", func(t *testing.T) {
		s := chatDownServer(t, cacheDir, chat.New(closedURL(t), "fake", 200*time.Millisecond))
		_, out, err := s.summarize(ctx, nil, SummarizeInput{Path: "auth.go", Mode: "summary", ProjectRoot: projRoot})
		if err != nil {
			t.Fatalf("read summary returned hard error with unreachable chat: %v", err)
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
