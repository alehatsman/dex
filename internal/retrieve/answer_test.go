package retrieve

import (
	"context"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/chat"
)

func TestAnswerCacheKeyDistinguishesFields(t *testing.T) {
	var c AnswerCache
	k1 := c.key("q", "intent", "model", "evidence")
	k2 := c.key("qi", "ntent", "model", "evidence") // same concat, different split
	if k1 == k2 {
		t.Error("cache key collided across field boundaries — length prefixing failed")
	}
}

func TestAnswerMaxTokensForGivesExplorationMoreRoom(t *testing.T) {
	targeted := answerMaxTokensFor(IntentSymbolLookup)
	for _, intent := range []string{IntentArchitecture, IntentPackageTopology} {
		if got := answerMaxTokensFor(intent); got <= targeted {
			t.Errorf("intent %q cap %d should exceed targeted cap %d (#568)", intent, got, targeted)
		}
	}
	if answerMaxTokensFor("auto") != targeted {
		t.Errorf("unknown intent should default to the targeted cap %d", targeted)
	}
}

// stubChatter returns a fixed Response.
type stubChatter struct {
	resp chat.Response
}

func (s *stubChatter) Generate(_ context.Context, _ []chat.Message, _ chat.Options) (chat.Response, error) {
	return s.resp, nil
}

func (s *stubChatter) GenerateStream(_ context.Context, _ []chat.Message, _ chat.Options, onToken func(string)) (chat.Response, error) {
	if onToken != nil {
		onToken(s.resp.Content)
	}
	return s.resp, nil
}

func (s *stubChatter) Endpoint() string             { return "stub" }
func (s *stubChatter) ModelName() string            { return "stub-model" }
func (s *stubChatter) Health(context.Context) error { return nil }

func TestSynthesizeAnswerMarksTruncation(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		wantMarker   bool
	}{
		{"length finish is marked", "length", true},
		{"stop finish is unmarked", "stop", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &stubChatter{resp: chat.Response{
				Content:      "partial answer that stops",
				FinishReason: tc.finishReason,
			}}
			var cache AnswerCache
			ans, _, err := SynthesizeAnswer(context.Background(), client, &cache, IntentArchitecture, "q", "evidence", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := strings.Contains(ans, strings.TrimSpace(answerTruncatedMarker))
			if got != tc.wantMarker {
				t.Errorf("marker present=%v, want %v — answer=%q", got, tc.wantMarker, ans)
			}
		})
	}
}

func TestSynthesizeAnswerStreamsTruncationMarker(t *testing.T) {
	client := &stubChatter{resp: chat.Response{Content: "partial", FinishReason: "length"}}
	var cache AnswerCache
	var streamed []string
	_, _, err := SynthesizeAnswer(context.Background(), client, &cache, IntentArchitecture, "q", "evidence",
		func(tok string) { streamed = append(streamed, tok) })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(streamed, "")
	if !strings.Contains(joined, strings.TrimSpace(answerTruncatedMarker)) {
		t.Errorf("streamed output missing truncation marker: %q", joined)
	}
}
