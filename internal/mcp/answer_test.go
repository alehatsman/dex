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
)

// resetAnswerCache clears the package-level cache so tests don't bleed
// into each other.
func resetAnswerCache(t *testing.T) {
	t.Helper()
	answerCacheMu.Lock()
	defer answerCacheMu.Unlock()
	answerCacheData = make(map[string]string, answerCacheCap)
	answerCacheOrder = answerCacheOrder[:0]
}

func TestSynthesizeAnswerPopulatesAnswer(t *testing.T) {
	resetAnswerCache(t)
	chatSrv := fakeChat(t, "The debounce lives in watch.go:42.")
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake-14b", 5*time.Second)}

	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{
			{Path: "internal/watch/watch.go", StartLine: 40, EndLine: 50, Reason: "debounce", Content: "func debounce() {}"},
		},
	}
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "where is debounce", out)

	if out.Answer == "" {
		t.Fatal("expected Answer to be populated")
	}
	if out.AnswerModel != "fake-14b" {
		t.Errorf("AnswerModel = %q, want fake-14b", out.AnswerModel)
	}
}

func TestSynthesizeAnswerNilChatClient(t *testing.T) {
	resetAnswerCache(t)
	s := &Server{} // no ChatClient
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "x"}},
	}
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "q", out)
	if out.Answer != "" {
		t.Errorf("expected no answer with nil chat client, got %q", out.Answer)
	}
}

func TestSynthesizeAnswerNoEvidence(t *testing.T) {
	resetAnswerCache(t)
	chatSrv := fakeChat(t, "should not be called")
	defer chatSrv.Close()
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake", 5*time.Second)}
	out := &ContextOutput{} // empty bundle
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "q", out)
	if out.Answer != "" {
		t.Errorf("expected no answer with empty evidence, got %q", out.Answer)
	}
}

func TestSynthesizeAnswerUnreachableDegrades(t *testing.T) {
	resetAnswerCache(t)
	// Point at a closed port so Generate returns ErrUnreachable.
	s := &Server{ChatClient: chat.New("http://127.0.0.1:1", "fake", 200*time.Millisecond)}
	out := &ContextOutput{
		Status:         "ok",
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "func A(){}"}},
	}
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "q", out)
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
	resetAnswerCache(t)
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
	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake", 5*time.Second)}

	mk := func() *ContextOutput {
		return &ContextOutput{SuggestedReads: []SuggestedRead{{Path: "a.go", StartLine: 1, EndLine: 2, Content: "func A(){}"}}}
	}
	o1 := mk()
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "same q", o1)
	o2 := mk()
	s.synthesizeAnswer(context.Background(), IntentBehaviorSearch, "same q", o2)

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
	ev := buildAnswerEvidence(out)
	if !strings.Contains(ev, "a.go:1-3") || !strings.Contains(ev, "AAA") {
		t.Errorf("evidence missing suggested read: %q", ev)
	}
	if !strings.Contains(ev, "Foo") || !strings.Contains(ev, "func Foo()") {
		t.Errorf("evidence missing symbol signature: %q", ev)
	}
}

func TestAnswerCacheKeyDistinguishesFields(t *testing.T) {
	k1 := answerCacheKey("q", "intent", "model", "evidence")
	k2 := answerCacheKey("qi", "ntent", "model", "evidence") // same concat, different split
	if k1 == k2 {
		t.Error("cache key collided across field boundaries — length prefixing failed")
	}
}

func TestExposeRawTools(t *testing.T) {
	cases := map[string]bool{"": false, "0": false, "off": false, "1": true, "true": true, "ON": true, "yes": true}
	for val, want := range cases {
		t.Setenv("DEX_EXPOSE_RAW_TOOLS", val)
		if got := exposeRawTools(); got != want {
			t.Errorf("exposeRawTools(%q) = %v, want %v", val, got, want)
		}
	}
}

func TestToolTierFromEnv(t *testing.T) {
	cases := []struct {
		dexTools string
		rawTools string
		wantTier toolTier
	}{
		{"", "", TierStandard}, // default
		{"standard", "", TierStandard},
		{"ask", "", TierAsk},
		{"power", "", TierPower},
		{"POWER", "", TierPower},      // case-insensitive
		{"unknown", "", TierStandard}, // unrecognised → default
		{"", "1", TierPower},          // legacy DEX_EXPOSE_RAW_TOOLS=1
		{"ask", "1", TierPower},       // legacy takes precedence
	}
	for _, tc := range cases {
		t.Setenv("DEX_TOOLS", tc.dexTools)
		t.Setenv("DEX_EXPOSE_RAW_TOOLS", tc.rawTools)
		if got := toolTierFromEnv(); got != tc.wantTier {
			t.Errorf("DEX_TOOLS=%q DEX_EXPOSE_RAW_TOOLS=%q → tier %d, want %d",
				tc.dexTools, tc.rawTools, got, tc.wantTier)
		}
	}
}

func TestBuildAnswerEvidenceSessionContext(t *testing.T) {
	out := &ContextOutput{
		SessionTask:    "refactor the watcher",
		KnowledgeFacts: []string{"[Architecture] watch.go owns the file watcher", "[Gotcha] debounce is 500ms"},
		SuggestedReads: []SuggestedRead{{Path: "watch.go", StartLine: 1, EndLine: 3, Content: "func Watch(){}"}},
	}
	ev := buildAnswerEvidence(out)

	if !strings.Contains(ev, "SESSION CONTEXT:") {
		t.Errorf("evidence missing SESSION CONTEXT block: %q", ev)
	}
	if !strings.Contains(ev, "Task: refactor the watcher") {
		t.Errorf("evidence missing session task: %q", ev)
	}
	if !strings.Contains(ev, "[Architecture] watch.go owns the file watcher") {
		t.Errorf("evidence missing knowledge fact: %q", ev)
	}
	// Session block must appear before code evidence.
	sessionIdx := strings.Index(ev, "SESSION CONTEXT:")
	codeIdx := strings.Index(ev, "watch.go")
	if sessionIdx >= codeIdx {
		t.Errorf("SESSION CONTEXT should precede code evidence (got positions %d vs %d)", sessionIdx, codeIdx)
	}
}

func TestBuildAnswerEvidenceNoSessionContext(t *testing.T) {
	out := &ContextOutput{
		SuggestedReads: []SuggestedRead{{Path: "a.go", Content: "func A(){}"}},
	}
	ev := buildAnswerEvidence(out)
	if strings.Contains(ev, "SESSION CONTEXT:") {
		t.Errorf("expected no SESSION CONTEXT block when session/knowledge absent: %q", ev)
	}
}
