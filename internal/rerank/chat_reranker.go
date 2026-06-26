package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// defaultInstruct is the task instruction embedded in the Qwen3-Reranker
// prompt. Code retrieval is the primary use case; callers may override via
// ChatReranker.Instruct for other domains.
const defaultInstruct = "Given a code search query, retrieve relevant code snippets that answer the query"

// logprobToken is one entry in a chat-completions top_logprobs list.
type logprobToken struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// ChatReranker scores (query, doc) pairs using a chat-completions endpoint
// with logprobs — the interface required by decoder-style rerankers such as
// Qwen3-Reranker-4B served by vLLM. It satisfies both Reranker and
// HealthChecker.
//
// Each pair is scored in a separate HTTP call. Calls are fanned out to
// Concurrency goroutines so the wall-clock cost is roughly the latency of
// the slowest pair rather than the sum of all pairs.
//
// Scoring: the model is prompted to answer "yes" or "no". When the server
// returns logprobs, the score is softmax(yes, no) ∈ [0, 1]; when logprobs
// are absent (non-vLLM servers), the score degrades to a binary
// yes=1 / no=0 / other=0.5.
//
// ThinkingPrefill: when true, the request includes an assistant prefill of
// "<think>\n\n</think>\n\n" and the vLLM-specific continue_final_message=true
// flag. This is required for Qwen3-style thinking models served on vLLM so
// max_tokens=1 lands directly on "yes"/"no". Leave false (default) for
// standard chat models and ollama — those ignore continue_final_message and
// instead continue the XML pattern, generating "<" not "yes"/"no".
type ChatReranker struct {
	BaseURL         string
	Model           string
	Instruct        string // task description in the prompt; defaults to defaultInstruct
	Concurrency     int    // max concurrent HTTP calls (default 4)
	ThinkingPrefill bool   // true = vLLM + Qwen3-Reranker; false (default) = ollama / standard chat
	HTTP            *http.Client
}

// NewChat creates a ChatReranker. concurrency ≤ 0 → 16; timeout ≤ 0 → 30 s.
func NewChat(baseURL, model string, concurrency int, timeout time.Duration) *ChatReranker {
	if concurrency <= 0 {
		concurrency = 16
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ChatReranker{
		BaseURL:     strings.TrimSuffix(baseURL, "/"),
		Model:       model,
		Instruct:    defaultInstruct,
		Concurrency: concurrency,
		HTTP:        &http.Client{Timeout: timeout},
	}
}

// Endpoint implements HealthChecker — returns the base URL for status reporting.
func (c *ChatReranker) Endpoint() string { return c.BaseURL }

// ModelName implements HealthChecker — returns the model identifier.
func (c *ChatReranker) ModelName() string { return c.Model }

// Rerank implements Reranker: scores every (query, doc) pair concurrently and
// returns results in descending-relevance order. Returns ErrUnreachable when
// every call fails at the transport layer.
func (c *ChatReranker) Rerank(ctx context.Context, query string, docs []string) ([]Score, error) {
	if len(docs) == 0 {
		return nil, nil
	}

	type result struct {
		score float32
		err   error
	}
	results := make([]result, len(docs))

	conc := c.Concurrency
	if conc <= 0 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, doc := range docs {
		wg.Add(1)
		go func(idx int, d string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			score, err := c.scoreOne(ctx, query, d)
			results[idx] = result{score: score, err: err}
		}(i, doc)
	}
	wg.Wait()

	// If every call failed, surface ErrUnreachable so store.Search can
	// degrade gracefully instead of presenting an empty result set.
	errCount := 0
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			errCount++
			if firstErr == nil {
				firstErr = r.err
			}
		}
	}
	if errCount == len(docs) {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, firstErr)
	}

	// Omit failed pairs rather than assigning score=0: a zero score would
	// place them above nothing but below every real result, obscuring which
	// hits were actually evaluated.
	var out []Score
	for i, r := range results {
		if r.err == nil {
			out = append(out, Score{Index: i, Score: r.score})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// Health implements HealthChecker. Hits GET /v1/models rather than
// scoring a real (query, doc) pair so a cold model load on the chat
// backend doesn't trip the short status-time timeout. The configured
// model not being loaded shows up on the first real rerank call, not
// in status.
func (c *ChatReranker) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("rerank(chat): /v1/models returned %d", resp.StatusCode)
	}
	return nil
}

// scoreOne sends a single (query, doc) pair to the chat completions endpoint
// and extracts a relevance score in [0, 1].
func (c *ChatReranker) scoreOne(ctx context.Context, query, doc string) (float32, error) {
	instruct := c.Instruct
	if instruct == "" {
		instruct = defaultInstruct
	}

	sysPrompt := "Judge whether the Document meets the requirements based on the Query and the Instruct. " +
		"Note that the answer can only be \"yes\" or \"no\"."
	userPrompt := fmt.Sprintf("<Instruct>: %s\n\n<Query>: %s\n\n<Document>: %s", instruct, query, doc)

	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgs := []message{
		{Role: "system", Content: sysPrompt},
		{Role: "user", Content: userPrompt},
	}
	reqMap := map[string]any{
		"model":        c.Model,
		"messages":     msgs,
		"max_tokens":   1,
		"temperature":  0,
		"logprobs":     true,
		"top_logprobs": 10,
	}
	// ThinkingPrefill: vLLM + Qwen3-Reranker needs an assistant prefill to
	// skip the <think>…</think> block so max_tokens=1 hits "yes"/"no" directly.
	// Standard servers (ollama, etc.) ignore continue_final_message but still
	// try to continue the XML pattern — producing "<" not "yes"/"no".
	if c.ThinkingPrefill {
		reqMap["messages"] = append(msgs, message{Role: "assistant", Content: "<think>\n\n</think>\n\n"})
		reqMap["continue_final_message"] = true
		reqMap["add_generation_prompt"] = false
	}

	body, err := json.Marshal(reqMap)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("chat rerank: http %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs *struct {
				Content []struct {
					Token       string         `json:"token"`
					Logprob     float64        `json:"logprob"`
					TopLogprobs []logprobToken `json:"top_logprobs"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, fmt.Errorf("chat rerank: decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return 0, fmt.Errorf("chat rerank: no choices in response")
	}
	ch := parsed.Choices[0]

	// Prefer logprobs (continuous, higher-resolution ranking).
	if ch.Logprobs != nil && len(ch.Logprobs.Content) > 0 {
		return yesNoFromLogprobs(ch.Logprobs.Content[0].TopLogprobs), nil
	}
	// Text fallback: binary yes=1 / no=0 / ambiguous=0.5.
	return yesNoFromText(ch.Message.Content), nil
}

// yesNoFromLogprobs extracts a [0,1] relevance score from top-token logprobs.
// Softmax over the "yes" and "no" tokens gives a proper probability estimate.
// Returns 0.5 (neutral) when neither token appears in the top candidates.
func yesNoFromLogprobs(tops []logprobToken) float32 {
	var yesLP, noLP float64
	hasYes, hasNo := false, false
	for _, t := range tops {
		switch strings.ToLower(strings.TrimSpace(t.Token)) {
		case "yes":
			yesLP = t.Logprob
			hasYes = true
		case "no":
			noLP = t.Logprob
			hasNo = true
		}
	}
	switch {
	case !hasYes && !hasNo:
		return 0.5
	case !hasYes:
		return 0
	case !hasNo:
		// Only "yes" visible — use sigmoid to cap at a sensible value.
		return float32(math.Min(math.Exp(yesLP), 1.0))
	default:
		eYes := math.Exp(yesLP)
		eNo := math.Exp(noLP)
		return float32(eYes / (eYes + eNo))
	}
}

// yesNoFromText parses plain "yes"/"no" from the generated text when logprobs
// are unavailable. Returns 0.5 for any other response so the hit is ordered
// between clear yes and clear no rather than discarded.
//
// Soft "no" containment is intentionally absent: "no" appears inside common
// identifiers (filename, node, unknown, annotation) and would produce scores
// lower than the neutral 0.5 on false matches — worse than doing nothing.
func yesNoFromText(content string) float32 {
	switch strings.ToLower(strings.TrimSpace(content)) {
	case "yes":
		return 1.0
	case "no":
		return 0.0
	default:
		if strings.Contains(strings.ToLower(content), "yes") {
			return 0.8
		}
		return 0.5
	}
}
