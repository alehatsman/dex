// Package rerank talks to a Cohere-shape /rerank endpoint (TEI,
// Infinity, vLLM with a reranker model). It is the relevance-side
// companion to internal/embed and internal/chat: where embed turns text
// into vectors for bi-encoder retrieval, rerank scores (query, doc)
// pairs jointly with a cross-encoder (cross-attention) to reorder the
// fused candidate pool that store.Search produces.
//
// The request body matches Cohere (`documents` + `top_n`), but the
// path is the open-source convention `/rerank`. Cohere's own API uses
// `/v1/rerank`; a proxy is needed to point this client at api.cohere.ai.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnreachable is returned when the rerank endpoint cannot be reached
// at the network layer. store.Search translates this into a graceful
// fall-through to non-reranked fused results, so upstream callers never
// see a search failure caused by reranker outages.
var ErrUnreachable = errors.New("rerank service unreachable")

type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// Score is one (document-index, relevance) pair returned by the server.
// Results are sorted descending by Score. Index refers to the position
// in the request `docs` slice — the caller maps back to chunk IDs.
type Score struct {
	Index int
	Score float32
}

// Reranker is the contract `store.Search` consumes — `(query, docs) →
// ranked scores`. *Client satisfies it; tests can swap in a stub that
// returns a deterministic permutation or `ErrUnreachable`.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]Score, error)
}

// New builds a client. baseURL is the server root (e.g.
// http://127.0.0.1:8082), not the /rerank path.
func New(baseURL, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

type rerankRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float32 `json:"relevance_score"`
	} `json:"results"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Rerank sends (query, docs) to /rerank and returns the server's
// ordering of indices into docs along with relevance scores. Results
// come back in descending-relevance order.
//
// Returns ErrUnreachable for dial/timeout failures so the caller can
// degrade to non-reranked results. Empty docs short-circuits with no
// network call. Out-of-range indices in the response are silently
// dropped — defensive against a buggy or version-mismatched server.
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]Score, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	reqBody := rerankRequest{
		Model:     c.Model,
		Query:     query,
		Documents: docs,
		TopN:      len(docs),
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(buf))
		// 5xx and 429 are reachability-shaped outages — an overloaded,
		// restarting, or gateway-fronted backend that is up enough to
		// answer but cannot serve. Wrap them as ErrUnreachable so the
		// breaker counts them toward tripping and store.Search degrades
		// to non-reranked results instead of failing the whole query
		// (#445). Other 4xx (bad model, wrong path, malformed request)
		// are configuration bugs we surface every time, per the breaker's
		// design (see breaker.go) — they are not wrapped.
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: http %d: %s", ErrUnreachable, resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("rerank: http %d: %s", resp.StatusCode, msg)
	}
	var parsed rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("rerank: decode: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("rerank: server error: %s", parsed.Error.Message)
	}
	out := make([]Score, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			continue
		}
		out = append(out, Score{Index: r.Index, Score: r.RelevanceScore})
	}
	return out, nil
}

// Health does a cheap liveness probe: a GET against /health, the near-
// universal reachability path served by TEI, Infinity, and vLLM reranker
// backends. It deliberately avoids a real Rerank() call: cross-encoder
// rerankers cold-load the model on the first inference (seconds on MPS/GPU),
// which routinely exceeds the short status-probe timeout and falsely reports
// the endpoint as UNREACHABLE even when the server is up and answering
// metadata requests instantly (#78) — the same cold-model trap embed.Client
// and chat.Client already dodge by probing /v1/models.
//
// The probe is lenient: any HTTP response — including a 404 from a backend
// that doesn't expose /health — proves the server is listening and speaking
// HTTP, so it counts as reachable. Only a transport-level failure (dial /
// timeout) is treated as unreachable. Whether the model is actually loaded
// surfaces on the first real rerank call, where the breaker (see breaker.go)
// degrades search to non-reranked results (#445) rather than failing it.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	_ = resp.Body.Close()
	return nil
}

// Endpoint returns the server's base URL for status reporting.
func (c *Client) Endpoint() string { return c.BaseURL }

// ModelName returns the configured model identifier for status reporting.
func (c *Client) ModelName() string { return c.Model }

// HealthChecker extends Reranker with endpoint metadata and a health probe.
// Both *Client and *ChatReranker implement it; mcp.Server stores this
// interface so either backend can be wired without a type switch.
type HealthChecker interface {
	Reranker
	Health(ctx context.Context) error
	Endpoint() string
	ModelName() string
}
