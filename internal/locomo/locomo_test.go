package locomo

import (
	"strings"
	"testing"
)

func TestTokenF1(t *testing.T) {
	cases := []struct {
		hyp, ref string
		want     float64
	}{
		{"the quick brown fox", "the quick brown fox", 1.0},
		{"the quick brown fox", "the slow brown dog", 0.5},    // 2/4 overlap
		{"", "", 1.0},
		{"hello world", "", 0.0},
		{"", "hello", 0.0},
		{"Paris is the capital of France", "Paris", 2.0 / 7.0}, // precision=1/6, recall=1/1 → 2*(1/6*1)/(1/6+1)=2/7≈0.286
	}
	for _, c := range cases {
		got := TokenF1(c.hyp, c.ref)
		if abs(got-c.want) > 0.01 {
			t.Errorf("TokenF1(%q, %q) = %.4f, want %.4f", c.hyp, c.ref, got, c.want)
		}
	}
}

func TestExactMatch(t *testing.T) {
	if !ExactMatch("Hello, World!", "hello world") {
		t.Error("expected match after normalization")
	}
	if ExactMatch("hello world", "hello") {
		t.Error("substring should not be exact match")
	}
}

func TestContains(t *testing.T) {
	if !Contains("Alice was born in Paris.", "paris") {
		t.Error("expected contains to find 'paris'")
	}
	if Contains("Alice was born in Paris.", "london") {
		t.Error("should not find 'london'")
	}
}

func TestLoadNDJSON(t *testing.T) {
	ndjson := `{"id":"c1","turns":[{"speaker":"A","text":"Hello"},{"speaker":"B","text":"Hi"}],"questions":[{"id":"q1","category":"single_hop","text":"Who said hello?","answer":"A","conv_id":"c1"}]}
{"id":"c2","turns":[{"speaker":"X","text":"Bye"}],"questions":[{"id":"q2","category":"temporal","text":"What did X say?","answer":"Bye","conv_id":"c2"}]}
`
	d, err := LoadNDJSON(strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("LoadNDJSON: %v", err)
	}
	if len(d.Conversations) != 2 {
		t.Fatalf("want 2 conversations, got %d", len(d.Conversations))
	}
	if d.Conversations[0].ID != "c1" {
		t.Errorf("want conv id c1, got %q", d.Conversations[0].ID)
	}
	if len(d.Questions()) != 2 {
		t.Errorf("want 2 questions, got %d", len(d.Questions()))
	}
}

func TestComputeReport(t *testing.T) {
	results := []QuestionResult{
		{Category: "single_hop", RecallAtK: true, BestTokenF1: 0.8, AnyExactMatch: false,
			TranscriptTokens: 100, RetrievedTokens: 20},
		{Category: "single_hop", RecallAtK: false, BestTokenF1: 0.2, AnyExactMatch: false,
			TranscriptTokens: 100, RetrievedTokens: 30},
		{Category: "multi_hop", RecallAtK: true, BestTokenF1: 1.0, AnyExactMatch: true,
			TranscriptTokens: 200, RetrievedTokens: 40},
	}
	rep := Compute(results, 5)
	if rep.K != 5 {
		t.Errorf("want k=5, got %d", rep.K)
	}
	if rep.Overall.N != 3 {
		t.Errorf("want 3 overall, got %d", rep.Overall.N)
	}
	if abs(rep.Overall.RecallAtK-2.0/3.0) > 0.001 {
		t.Errorf("recall@k want %.3f got %.3f", 2.0/3.0, rep.Overall.RecallAtK)
	}
	if len(rep.Categories) != 2 {
		t.Errorf("want 2 categories, got %d", len(rep.Categories))
	}
	md := rep.Markdown()
	if !strings.Contains(md, "LoCoMo") {
		t.Error("markdown missing header")
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
