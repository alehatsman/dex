// Package embed talks to an OpenAI-compatible /v1/embeddings endpoint
// (vLLM, TEI's compat shim, ollama, …). It batches inputs and returns
// packed float32 vectors.
package embed

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

	"golang.org/x/sync/errgroup"
)

// ErrUnreachable is returned when the embed endpoint cannot be reached at
// the network layer. The MCP server translates this into a structured
// "embedding-service-unreachable" result so Claude can fall back to grep.
var ErrUnreachable = errors.New("embedding service unreachable")

// Embedder is the embedding backend contract the rest of dex depends on.
// *Client (the OpenAI-compatible HTTP backend) is the only implementation
// today; the interface is the seam an in-process engine (e.g. an optional
// ONNX backend) plugs into without touching store/index/mcp. Backends signal
// a network-layer outage by returning ErrUnreachable so callers degrade
// gracefully — that contract is part of this interface.
type Embedder interface {
	// Embed returns one vector per input, in input order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// Health is a cheap reachability probe.
	Health(ctx context.Context) error
	// Endpoint and ModelName are metadata for status reporting.
	Endpoint() string
	ModelName() string
	// BatchSize is the per-call chunk size the indexer loops over so it can
	// embed-and-upsert one batch at a time (crash-survival, see index.Run).
	BatchSize() int
	// EmbedConcurrency is how many BatchSize-sized sub-batches this embedder
	// dispatches in flight per Embed call. The indexer multiplies it into a
	// super-batch (BatchSize × EmbedConcurrency) so a single Embed call
	// actually has enough sub-batches to fan out — see index.runEmbedPhase.
	// <=1 means sequential (onnx, opted-out HTTP clients).
	EmbedConcurrency() int
}

// Compile-time check that *Client satisfies Embedder.
var _ Embedder = (*Client)(nil)

type Client struct {
	BaseURL string
	Model   string
	Batch   int
	// Concurrency caps in-flight /v1/embeddings calls. <=1 = sequential
	// (the historical behaviour). Larger values let multiple batches
	// overlap network RTT with GPU work on servers that handle
	// concurrent requests (vLLM, TEI, Ollama, …). The HTTP transport is
	// sized for this — see New().
	Concurrency int
	// MaxRetries bounds how many times a single batch is retried after a
	// *transient* failure (network error, HTTP 5xx, or 429) before giving
	// up. A single transient blip (brief server restart, connection reset,
	// rate-limit spike) would otherwise abort the whole indexing pass. 0 =
	// no retry. Deterministic failures (4xx, decode errors, malformed
	// responses) are never retried. Set by NewWithConcurrency.
	MaxRetries int
	// RetryBaseDelay is the first backoff interval; it doubles each attempt
	// (RetryBaseDelay, 2×, 4×, …). Backoff sleeps honor context cancellation.
	RetryBaseDelay time.Duration
	HTTP           *http.Client
}

// New builds a client. baseURL is the server root (e.g.
// http://127.0.0.1:8082), not the /v1/embeddings path.
func New(baseURL, model string, batch int, timeout time.Duration) *Client {
	return NewWithConcurrency(baseURL, model, batch, 1, timeout)
}

// NewWithConcurrency is like New but also pins the in-flight call limit.
// concurrency<=0 falls back to 1 (sequential).
func NewWithConcurrency(baseURL, model string, batch, concurrency int, timeout time.Duration) *Client {
	if batch <= 0 {
		batch = 32
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	// Default http.Transport keeps only 2 idle conns per host, which
	// throttles parallel dispatch to the same embedding server. Size
	// the pool to the configured concurrency so workers don't dial a
	// fresh TCP/TLS connection on every batch.
	//
	// http.DefaultTransport is documented to be *http.Transport; the
	// comma-ok form satisfies errcheck and degrades to a fresh-default
	// Transport on the off chance a future stdlib change broke that.
	base, ok := http.DefaultTransport.(*http.Transport)
	var tr *http.Transport
	if ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.MaxIdleConns = concurrency * 2
	tr.MaxIdleConnsPerHost = concurrency * 2
	tr.MaxConnsPerHost = concurrency * 2
	return &Client{
		BaseURL:        strings.TrimSuffix(baseURL, "/"),
		Model:          model,
		Batch:          batch,
		Concurrency:    concurrency,
		MaxRetries:     3,
		RetryBaseDelay: 250 * time.Millisecond,
		HTTP:           &http.Client{Timeout: timeout, Transport: tr},
	}
}

func (c *Client) Endpoint() string       { return c.BaseURL }
func (c *Client) ModelName() string      { return c.Model }
func (c *Client) BatchSize() int         { return c.Batch }
func (c *Client) EmbedConcurrency() int  { return c.Concurrency }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed sends inputs in batches of c.Batch and returns one vector per input.
// Up to c.Concurrency batches are in flight at once; output ordering is
// preserved regardless (each batch writes its own slot in `out`).
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(inputs))
	conc := c.Concurrency
	if conc <= 1 {
		// Sequential fast-path: skips goroutine/errgroup overhead for
		// callers that opted out of parallel dispatch (Concurrency<=1).
		for start := 0; start < len(inputs); start += c.Batch {
			end := start + c.Batch
			if end > len(inputs) {
				end = len(inputs)
			}
			got, err := c.embedBatch(ctx, inputs[start:end])
			if err != nil {
				return nil, err
			}
			if len(got) != end-start {
				return nil, fmt.Errorf("embed: server returned %d vectors for %d inputs", len(got), end-start)
			}
			copy(out[start:end], got)
		}
		if err := validateVectors(out); err != nil {
			return nil, err
		}
		return out, nil
	}
	eg, egctx := errgroup.WithContext(ctx)
	eg.SetLimit(conc)
	for start := 0; start < len(inputs); start += c.Batch {
		end := start + c.Batch
		if end > len(inputs) {
			end = len(inputs)
		}
		start, end := start, end
		eg.Go(func() error {
			got, err := c.embedBatch(egctx, inputs[start:end])
			if err != nil {
				return err
			}
			if len(got) != end-start {
				return fmt.Errorf("embed: server returned %d vectors for %d inputs", len(got), end-start)
			}
			copy(out[start:end], got)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	if err := validateVectors(out); err != nil {
		return nil, err
	}
	return out, nil
}

// validateVectors rejects corrupt embeddings before they reach the store.
// EnsureEmbedModel guards only the model-name string recorded in meta, and
// the per-batch length check counts vectors without inspecting them — so a
// server quietly serving a truncated/different model, returning an empty
// "embedding":[] for one item, or repeating an `index` (leaving another
// slot nil) would otherwise write zero/wrong-width vectors into the index
// and silently degrade cosine search until a manual reindex (#435).
//
// The HTTP backend has no externally configured dimension (unlike the ONNX
// engine's DEX_ONNX_DIM), so width is validated for self-consistency: every
// vector in the response must share one non-zero width, and none may be
// uniformly zero. Non-finite components need no check here — encoding/json
// rejects out-of-range numbers at decode and JSON has no NaN/Inf literal,
// so they can never reach this point.
func validateVectors(vecs [][]float32) error {
	dim := 0
	for i, v := range vecs {
		if len(v) == 0 {
			return fmt.Errorf("embed: missing or empty vector at index %d", i)
		}
		if dim == 0 {
			dim = len(v)
		} else if len(v) != dim {
			return fmt.Errorf("embed: inconsistent vector width at index %d: got %d, want %d", i, len(v), dim)
		}
		allZero := true
		for _, f := range v {
			if f != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return fmt.Errorf("embed: all-zero vector at index %d", i)
		}
	}
	return nil
}

// embedBatch sends one batch, retrying up to c.MaxRetries times on a transient
// failure (network error, HTTP 5xx, or 429) with exponential backoff. A single
// transient blip would otherwise abort the entire indexing pass. Deterministic
// failures (4xx, decode/parse errors) return immediately — retrying them only
// wastes time. Backoff sleeps stop early on context cancellation.
func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	for attempt := 0; ; attempt++ {
		out, retryable, err := c.embedBatchOnce(ctx, inputs)
		if err == nil {
			return out, nil
		}
		if !retryable || attempt >= c.MaxRetries {
			return nil, err
		}
		// Exponential backoff: RetryBaseDelay, 2×, 4×, … honoring ctx.
		delay := c.RetryBaseDelay << attempt
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// embedBatchOnce performs a single embed request. The bool reports whether a
// non-nil error is transient (safe to retry): network-layer failures and HTTP
// 5xx/429. 4xx and response-parsing errors are deterministic and not retried.
func (c *Client) embedBatchOnce(ctx context.Context, inputs []string) ([][]float32, bool, error) {
	body, err := json.Marshal(embedRequest{Model: c.Model, Input: inputs})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Network-layer failure (conn refused/reset, timeout): transient.
		return nil, true, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// 5xx (server-side) and 429 (rate limit) are transient; 4xx is a
		// client-side error that will fail identically on retry.
		retryable := resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retryable, fmt.Errorf("embed: http %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, false, fmt.Errorf("embed: decode: %w", err)
	}
	if parsed.Error != nil {
		return nil, false, fmt.Errorf("embed: server error: %s", parsed.Error.Message)
	}
	out := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, false, fmt.Errorf("embed: bogus index %d in response", d.Index)
		}
		if out[d.Index] != nil {
			return nil, false, fmt.Errorf("embed: duplicate index %d in response", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, false, nil
}

// Health does a cheap reachability check via GET /v1/models — a metadata
// endpoint every OpenAI-compatible backend (ollama, vLLM, TEI) serves
// instantly without touching the model. Using a live embed call here would
// send real inference through ollama, which queues behind large pinned models
// and can take 5+ seconds — long enough to exceed the 3s status-probe timeout
// and falsely report the endpoint as UNREACHABLE even when it is functional.
// Whether the configured model is actually loaded surfaces on the first real
// embed call, consistent with how chat.Client.Health() behaves.
func (c *Client) Health(ctx context.Context) error {
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
		return fmt.Errorf("embed: /v1/models returned %d", resp.StatusCode)
	}
	return nil
}
