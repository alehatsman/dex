package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/retrieve"
)

func TestSynthesizeAnswerPopulatesAnswer(t *testing.T) {
	chatSrv := fakeChat(t, "The debounce lives in watch.go:42.")
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake-14b", 5*time.Second), ChatConfigured: true}

	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{
			{Path: "internal/watch/watch.go", StartLine: 40, EndLine: 50, Reason: "debounce", Content: "func debounce() {}"},
		},
	}
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "where is debounce", out)

	if out.Answer == "" {
		t.Fatal("expected Answer to be populated")
	}
	if out.AnswerModel != "fake-14b" {
		t.Errorf("AnswerModel = %q, want fake-14b", out.AnswerModel)
	}
}

// fakeStreamChat serves the given tokens as an SSE /v1/chat/completions
// stream, so GenerateStream (the logTok path) delivers them one by one.
func fakeStreamChat(t *testing.T, tokens []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, tok := range tokens {
			b, _ := json.Marshal(map[string]any{
				"model":   "fake-14b",
				"choices": []map[string]any{{"delta": map[string]string{"content": tok}}},
			})
			_, _ = w.Write([]byte("data: " + string(b) + "\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n"))
		if fl != nil {
			fl.Flush()
		}
	}))
}

// TestSynthesizeAnswerStreamsTokens asserts that a non-nil logTok receives
// the answer token-by-token (the CLI streaming contract, #565) and that the
// assembled out.Answer begins with the streamed text — the invariant the CLI
// relies on to print only the post-stream suffix.
func TestSynthesizeAnswerStreamsTokens(t *testing.T) {
	tokens := []string{"The ", "debounce ", "lives ", "in ", "internal/watch/watch.go:42."}
	chatSrv := fakeStreamChat(t, tokens)
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake-14b", 5*time.Second), ChatConfigured: true}

	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{
			{Path: "internal/watch/watch.go", StartLine: 40, EndLine: 50, Reason: "debounce", Content: "func debounce() {}"},
		},
	}
	var got []string
	sink := func(tok string) { got = append(got, tok) }
	s.synthesizeAnswer(context.Background(), sink, retrieve.IntentBehaviorSearch, "where is debounce", out)

	if len(got) <= 1 {
		t.Fatalf("expected multiple streamed tokens, got %d (%q) — sink not wired to GenerateStream", len(got), got)
	}
	streamed := strings.Join(got, "")
	if !strings.HasPrefix(out.Answer, strings.TrimSpace(streamed)) {
		t.Errorf("out.Answer %q must begin with streamed text %q", out.Answer, strings.TrimSpace(streamed))
	}
}

func TestSynthesizeAnswerNilChatClient(t *testing.T) {

	s := &Server{} // no ChatClient
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "x"}},
	}
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "q", out)
	if out.Answer != "" {
		t.Errorf("expected no answer with nil chat client, got %q", out.Answer)
	}
}

func TestSynthesizeAnswerNoEvidence(t *testing.T) {

	chatSrv := fakeChat(t, "should not be called")
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake", 5*time.Second), ChatConfigured: true}
	out := &ContextOutput{} // empty bundle
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "q", out)
	if out.Answer != "" {
		t.Errorf("expected no answer with empty evidence, got %q", out.Answer)
	}
}

func TestSynthesizeAnswerUnreachableDegrades(t *testing.T) {

	// Point at a closed port so Generate returns ErrUnreachable.
	s := &Server{ChatClient: chat.New(closedURL(t), "fake", 200*time.Millisecond), ChatConfigured: true}
	out := &ContextOutput{
		Status:         "ok",
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "func A(){}"}},
	}
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "q", out)
	if out.Answer != "" {
		t.Errorf("expected empty answer on unreachable chat, got %q", out.Answer)
	}
	// Status must be untouched — synthesis failure never breaks ask.
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok (synthesis must not mutate status)", out.Status)
	}
}

// TestSynthesizeAnswerCacheHit asserts identical evidence re-asks the
// model only once: the second call is served from cache.
func TestSynthesizeAnswerCacheHit(t *testing.T) {

	var calls atomic.Int32
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "cached answer"}, "finish_reason": "stop"},
			},
			"model": "fake",
		})
	}))
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake", 5*time.Second), ChatConfigured: true}

	mk := func() *ContextOutput {
		return &ContextOutput{SuggestedReads: []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 2, Content: "func A(){}"}}}
	}
	o1 := mk()
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "same q", o1)
	o2 := mk()
	s.synthesizeAnswer(context.Background(), nil, retrieve.IntentBehaviorSearch, "same q", o2)

	if o1.Answer != "cached answer" || o2.Answer != "cached answer" {
		t.Fatalf("answers = %q / %q, want both 'cached answer'", o1.Answer, o2.Answer)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("chat called %d times, want 1 (second should be a cache hit)", got)
	}
}

func TestBuildAnswerEvidenceOrderingAndBudget(t *testing.T) {
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 3, Reason: "primary", Content: "AAA"}},
		Symbols:        []SymbolHit{{QualifiedName: "Foo", Kind: "function", Path: "a.go", StartLine: 1, Signature: "func Foo()"}},
	}
	ev := buildAnswerEvidence(retrieve.IntentBehaviorSearch, out)
	if !strings.Contains(ev, "a.go:1-3") || !strings.Contains(ev, "AAA") {
		t.Errorf("evidence missing suggested read: %q", ev)
	}
	if !strings.Contains(ev, "Foo") || !strings.Contains(ev, "func Foo()") {
		t.Errorf("evidence missing symbol signature: %q", ev)
	}
}

// TestBuildAnswerEvidenceCallersLeadWithGraph asserts that for the
// callers/callees intents the GRAPH EDGES — the authoritative answer — are
// rendered even when a budget-filling reads block precedes them. Regression
// for #535: edges rendered last got truncated out, so the model answered
// "no callers" while graph.edges carried the real two.
func TestBuildAnswerEvidenceCallersLeadWithGraph(t *testing.T) {
	// A reads payload comfortably larger than the evidence byte budget, so
	// the old last-place graph rendering would be truncated away entirely.
	huge := strings.Repeat("X", answerMaxEvidenceBytes+4096)
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "big.go", StartLine: 1, EndLine: 9, Content: huge}},
		Graph: &GraphResult{Edges: []GraphEdge{
			{From: "store.(*Store).Search", To: "store.(*Store).SearchFused", Kind: "calls"},
			{From: "mcp.(*Server).search", To: "store.(*Store).SearchFused", Kind: "calls"},
		}},
	}

	for _, intent := range []string{retrieve.IntentCallers, retrieve.IntentCallees} {
		ev := buildAnswerEvidence(intent, out)
		if !strings.Contains(ev, "GRAPH EDGES:") {
			t.Errorf("intent %s: graph edges truncated out of evidence", intent)
		}
		if !strings.Contains(ev, "store.(*Store).Search --calls--> store.(*Store).SearchFused") {
			t.Errorf("intent %s: caller edge missing from evidence", intent)
		}
		// Edges must precede the bulky reads payload (they lead).
		if gi, ri := strings.Index(ev, "GRAPH EDGES:"), strings.Index(ev, "big.go"); ri >= 0 && gi > ri {
			t.Errorf("intent %s: graph edges should lead, got edge@%d after read@%d", intent, gi, ri)
		}
	}

	// For a non-graph intent the edges keep their trailing position.
	ev := buildAnswerEvidence(retrieve.IntentBehaviorSearch, out)
	if gi, ri := strings.Index(ev, "GRAPH EDGES:"), strings.Index(ev, "big.go"); gi >= 0 && ri >= 0 && gi < ri {
		t.Errorf("behavior_search: graph edges should trail reads, got edge@%d before read@%d", gi, ri)
	}
}

// TestAnswerCacheKeyDistinguishesFields moved to internal/retrieve
// (answer_test.go) with the AnswerCache type.

func TestBuildAnswerEvidenceSessionContext(t *testing.T) {
	out := &ContextOutput{
		SessionTask:    "refactor the watcher",
		SuggestedReads: []SuggestedRead{{Path: "watch.go", StartLine: 1, EndLine: 3, Content: "func Watch(){}"}},
	}
	ev := buildAnswerEvidence(retrieve.IntentBehaviorSearch, out)

	if !strings.Contains(ev, "SESSION CONTEXT:") {
		t.Errorf("evidence missing SESSION CONTEXT block: %q", ev)
	}
	if !strings.Contains(ev, "Task: refactor the watcher") {
		t.Errorf("evidence missing session task: %q", ev)
	}
	// Session block must appear after code evidence (KV-cache: stable code prefix, dynamic tail).
	sessionIdx := strings.Index(ev, "SESSION CONTEXT:")
	codeIdx := strings.Index(ev, "watch.go")
	if sessionIdx <= codeIdx {
		t.Errorf("SESSION CONTEXT should follow code evidence (got positions %d vs %d)", sessionIdx, codeIdx)
	}
}

func TestBuildAnswerEvidenceNoSessionContext(t *testing.T) {
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "func A(){}"}},
	}
	ev := buildAnswerEvidence(retrieve.IntentBehaviorSearch, out)
	if strings.Contains(ev, "SESSION CONTEXT:") {
		t.Errorf("expected no SESSION CONTEXT block when session/knowledge absent: %q", ev)
	}
}
