package locomo

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"testing"
)

func TestTokenF1(t *testing.T) {
	cases := []struct {
		hyp, ref string
		want     float64
	}{
		{"the quick brown fox", "the quick brown fox", 1.0},
		{"the quick brown fox", "the slow brown dog", 0.5}, // 2/4 overlap
		{"", "", 1.0},
		{"hello world", "", 0.0},
		{"", "hello", 0.0},
		{"Paris is the capital of France", "Paris", 2.0 / 7.0}, // precision=1/6, recall=1/1 → 2*(1/6*1)/(1/6+1)=2/7≈0.286
	}
	for _, c := range cases {
		got := TokenF1(c.hyp, c.ref)
		if math.Abs(got-c.want) > 0.01 {
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
	if math.Abs(rep.Overall.RecallAtK-2.0/3.0) > 0.001 {
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

// fakeEmbedder returns a deterministic 32-dim unit vector per input text,
// using FNV hashing so semantically related turns can be steered to similar
// vectors by crafting inputs — but for this test simple uniqueness suffices.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		out[i] = hashVec(s, 32)
	}
	return out, nil
}
func (fakeEmbedder) Health(_ context.Context) error { return nil }
func (fakeEmbedder) Endpoint() string               { return "fake" }
func (fakeEmbedder) ModelName() string              { return "fake" }
func (fakeEmbedder) BatchSize() int                 { return 64 }
func (fakeEmbedder) EmbedConcurrency() int          { return 1 }

func hashVec(s string, dim int) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	sum := h.Sum64()
	v := make([]float32, dim)
	for i := range v {
		h2 := fnv.New64a()
		_, _ = h2.Write([]byte{byte(i)})
		h2.Write([]byte(s))
		v[i] = float32(int64(h2.Sum64()%1000)-500) / 500.0
		_ = sum
	}
	// L2-normalize.
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range v {
			v[i] /= float32(norm)
		}
	}
	return v
}

func TestRun(t *testing.T) {
	ndjson := `{"id":"c1","turns":[` +
		`{"speaker":"Alice","text":"I visited Paris last summer and saw the Eiffel Tower."},` +
		`{"speaker":"Bob","text":"That sounds amazing! Did you try French cuisine?"},` +
		`{"speaker":"Alice","text":"Yes, I had croissants every morning near the Seine."},` +
		`{"speaker":"Bob","text":"How long did you stay?"},` +
		`{"speaker":"Alice","text":"I stayed for ten days and visited the Louvre twice."},` +
		`{"speaker":"Bob","text":"Did you visit any other cities?"},` +
		`{"speaker":"Alice","text":"I also spent two days in Lyon for the food scene."}` +
		`],"questions":[` +
		`{"id":"q1","category":"single_hop","text":"Which landmark did Alice visit in Paris?","answer":"Eiffel Tower","conv_id":"c1"},` +
		`{"id":"q2","category":"single_hop","text":"How long did Alice stay?","answer":"ten days","conv_id":"c1"}` +
		`]}`
	d, err := LoadNDJSON(strings.NewReader(ndjson))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	results, err := Run(context.Background(), fakeEmbedder{}, d, 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.TranscriptTokens == 0 {
			t.Errorf("q %q: TranscriptTokens=0", r.QuestionID)
		}
		if r.RetrievedTokens == 0 {
			t.Errorf("q %q: RetrievedTokens=0", r.QuestionID)
		}
		if len(r.TopK) != 3 {
			t.Errorf("q %q: want 3 hits, got %d", r.QuestionID, len(r.TopK))
		}
	}
	rep := Compute(results, 3)
	if rep.Overall.AvgTokenSavings == 0 {
		t.Errorf("AvgTokenSavings=0: transcript=%d retrieved=%d",
			results[0].TranscriptTokens, results[0].RetrievedTokens)
	}
}
