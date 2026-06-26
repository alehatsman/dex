// Package chat talks to an OpenAI-compatible /v1/chat/completions endpoint
// (vLLM, TEI's compat shim, ollama, …). It is the generation-side companion
// to internal/embed: where embed turns text into vectors for retrieval,
// chat turns a prompt + retrieved context into a model completion.
package chat

import (
	"bufio"
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

// ErrUnreachable is returned when the chat endpoint cannot be reached at
// the network layer. The MCP server translates this into a structured
// "chat-service-unreachable" result so Claude can surface the failure
// cleanly instead of pretending success.
var ErrUnreachable = errors.New("chat service unreachable")

// ErrModelNotFound is returned by Health when the endpoint is reachable
// but the configured model does not appear in the /v1/models listing.
var ErrModelNotFound = errors.New("chat model not found")

// ErrStreamTruncated is returned by GenerateStream when the SSE stream
// ends cleanly (EOF without a connection error) but the model never sent
// a finish_reason. This indicates the model server died mid-generation
// (OOM kill, graceful restart) and the partial response must not be
// treated as a complete answer.
var ErrStreamTruncated = errors.New("chat: stream truncated (no finish_reason; model server may have died mid-generation)")

// idleStreamTimeout is the maximum time to wait between consecutive SSE
// tokens before treating the stream as stalled. A GPU OOM stall or hung
// inference worker can hold the connection open without sending any data;
// this guard prevents the MCP session from blocking forever.
const idleStreamTimeout = 2 * time.Minute

// Chatter is the interface satisfied by *Client. Callers that need to
// inject a stub in tests should hold a Chatter, not a *Client.
// Mirrors the embed.Embedder pattern in internal/embed/client.go.
type Chatter interface {
	Generate(ctx context.Context, msgs []Message, opts Options) (Response, error)
	GenerateStream(ctx context.Context, msgs []Message, opts Options, onToken func(string)) (Response, error)
	Endpoint() string
	ModelName() string
	Health(ctx context.Context) error
}

type Client struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// New builds a client. baseURL is the server root (e.g.
// http://127.0.0.1:8082), not the /v1/chat/completions path.
func New(baseURL, model string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) Endpoint() string  { return c.BaseURL }
func (c *Client) ModelName() string { return c.Model }

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Options struct {
	// Temperature in [0, 2]. Zero means "use server default" (we omit
	// the field rather than forcing greedy decoding).
	Temperature float32
	// MaxTokens caps the response length. Zero means "use server default".
	MaxTokens int
	// Model overrides the client's default Model for this call. Empty
	// means "use c.Model". Lets a single Client serve multiple tools
	// that each want a different model on the same backend — e.g.
	// generate_code on a coder model, ask_codebase on an instruct model.
	Model string
	// ReasoningEffort maps to the OpenAI-compatible reasoning_effort field.
	// Set "none" to disable a thinking model's reasoning trace (qwen3.x via
	// ollama) so the answer lands in content, not a separate reasoning
	// channel. Empty omits the field (server default).
	ReasoningEffort string
}

type Response struct {
	Content      string
	Model        string
	FinishReason string
}

type chatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Temperature     *float32  `json:"temperature,omitempty"`
	MaxTokens       *int      `json:"max_tokens,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Stream          bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Model string `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Model string `json:"model"`
}

// Generate sends messages to the chat endpoint and returns the first
// choice. We don't stream — the MCP tool returns once per call, so the
// extra plumbing wouldn't change anything user-visible.
func (c *Client) Generate(ctx context.Context, messages []Message, opts Options) (Response, error) {
	if len(messages) == 0 {
		return Response{}, fmt.Errorf("chat: no messages")
	}
	model := c.Model
	if opts.Model != "" {
		model = opts.Model
	}
	reqBody := chatRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
	}
	if opts.Temperature > 0 {
		t := opts.Temperature
		reqBody.Temperature = &t
	}
	if opts.MaxTokens > 0 {
		m := opts.MaxTokens
		reqBody.MaxTokens = &m
	}
	reqBody.ReasoningEffort = opts.ReasoningEffort
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Response{}, fmt.Errorf("chat: http %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Response{}, fmt.Errorf("chat: decode: %w", err)
	}
	if parsed.Error != nil {
		return Response{}, fmt.Errorf("chat: server error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("chat: server returned no choices")
	}
	return Response{
		Content:      parsed.Choices[0].Message.Content,
		Model:        parsed.Model,
		FinishReason: parsed.Choices[0].FinishReason,
	}, nil
}

// GenerateStream sends messages with stream=true and calls onToken for each
// content delta as it arrives. Returns the assembled Response once the stream
// ends. onToken is called synchronously from the read loop; it must not block.
func (c *Client) GenerateStream(ctx context.Context, messages []Message, opts Options, onToken func(string)) (Response, error) {
	if len(messages) == 0 {
		return Response{}, fmt.Errorf("chat: no messages")
	}
	model := c.Model
	if opts.Model != "" {
		model = opts.Model
	}
	reqBody := chatRequest{Model: model, Messages: messages, Stream: true}
	if opts.Temperature > 0 {
		t := opts.Temperature
		reqBody.Temperature = &t
	}
	if opts.MaxTokens > 0 {
		m := opts.MaxTokens
		reqBody.MaxTokens = &m
	}
	reqBody.ReasoningEffort = opts.ReasoningEffort
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Response{}, err
	}
	// Wrap the context with a cancel-cause so the idle watchdog below can
	// terminate the stream with an explanatory error without polluting the
	// caller's context.
	streamCtx, streamCancel := context.WithCancelCause(ctx)
	defer streamCancel(nil)

	// Idle watchdog: if no SSE token arrives within idleStreamTimeout,
	// cancel the stream. A GPU OOM stall or hung inference worker can hold
	// the TCP connection open indefinitely without sending data.
	idleReset := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idleStreamTimeout)
		defer t.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-idleReset:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(idleStreamTimeout)
			case <-t.C:
				streamCancel(fmt.Errorf("chat: stream idle for %v — GPU may be stalled", idleStreamTimeout))
				return
			}
		}
	}()

	// Use a transport-only client so the per-request Timeout on c.HTTP
	// doesn't fire during a healthy long generation; the idle watchdog
	// above guards against a genuinely stalled connection.
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	streamReq.Header.Set("Content-Type", "application/json")
	streamReq.Header.Set("Accept", "text/event-stream")
	streamClient := &http.Client{Transport: c.HTTP.Transport}
	resp, err := streamClient.Do(streamReq)
	if err != nil {
		if cause := context.Cause(streamCtx); cause != nil {
			return Response{}, cause
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Response{}, fmt.Errorf("chat: http %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var (
		sb           strings.Builder
		finishReason string
		respModel    string
		gotDONE      bool
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			gotDONE = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Model != "" {
			respModel = chunk.Model
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			if tok := ch.Delta.Content; tok != "" {
				sb.WriteString(tok)
				if onToken != nil {
					onToken(tok)
				}
				// Signal the idle watchdog that we received a token.
				select {
				case idleReset <- struct{}{}:
				default:
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if cause := context.Cause(streamCtx); cause != nil {
			return Response{}, cause
		}
		return Response{}, fmt.Errorf("chat: stream read: %w", err)
	}
	// Detect a clean-EOF truncation: the connection closed without [DONE]
	// (and without a finish_reason), which happens when the model server is
	// killed mid-generation (OOM, graceful restart). A partial response must
	// not be returned as a successful complete answer or cached.
	// Note: some servers omit finish_reason but do send [DONE]; gotDONE
	// distinguishes a clean protocol end from a TCP-close truncation.
	if !gotDONE && finishReason == "" && sb.Len() > 0 {
		return Response{}, ErrStreamTruncated
	}
	return Response{Content: sb.String(), Model: respModel, FinishReason: finishReason}, nil
}

// Health does a cheap reachability check: a GET against /v1/models.
// We deliberately avoid a real Generate() call because on Ollama / vLLM
// that triggers a cold model load and routinely exceeds the short
// status-time timeout, producing misleading UNREACHABLE rows when the
// service is fine. /v1/models is the OpenAI-compat listing endpoint;
// every supported backend (Ollama, vLLM, llama.cpp-server, TEI) serves
// it cheaply.
//
// Beyond raw reachability the method also parses the response body and
// verifies that c.Model appears in the listing. If the body cannot be
// decoded (proxy that omits the listing, non-standard format) we fail
// open and return nil — better to report "ok" than to block a working
// setup with an opaque parse error. When the list is readable but the
// model is absent we return ErrModelNotFound.
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
		return fmt.Errorf("chat: /v1/models returned %d", resp.StatusCode)
	}

	var modelList struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&modelList); err != nil {
		// Fail-open: proxy or non-standard endpoint that doesn't return
		// an enumerable list — treat as reachable.
		return nil
	}
	// Empty data list means the endpoint didn't enumerate models — fail
	// open so backends that return {"data":[]} don't show as degraded.
	if len(modelList.Data) == 0 {
		return nil
	}
	for _, m := range modelList.Data {
		if m.ID == c.Model {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrModelNotFound, c.Model)
}
