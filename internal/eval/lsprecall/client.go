// Package lsprecall provides a minimal LSP client for bench-only use.
// It is NOT a production client: no connection pooling, no idle timeout,
// no re-use across requests. Spawn → initialize → query → shutdown per run.
//
// Protocol: JSON-RPC 2.0 over stdio with the standard LSP framing
// "Content-Length: N\r\n\r\n{...}".
package lsprecall

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a one-shot LSP client. Create with Spawn; call Shutdown when done.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID atomic.Int64
}

// Spawn launches the LSP server at the given command and waits for it to be
// ready (initialize handshake). rootURI must be a file:// URI for the project.
func Spawn(ctx context.Context, command []string, rootURI string) (*Client, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("lsprecall: empty server command")
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsprecall: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsprecall: stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsprecall: start %q: %w", command[0], err)
	}
	c := &Client{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}
	if err := c.initialize(rootURI); err != nil {
		_ = c.Shutdown()
		return nil, fmt.Errorf("lsprecall: initialize: %w", err)
	}
	return c, nil
}

// Shutdown sends shutdown + exit and waits for the process to terminate.
func (c *Client) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.call(ctx, "shutdown", nil, nil)
	_ = c.notify("exit", nil)
	_ = c.stdin.Close()
	return c.cmd.Wait()
}

// DidOpen notifies the server that a file is open (required before queries).
func (c *Client) DidOpen(ctx context.Context, uri, languageID, text string) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	})
}

// Location is a source location returned by LSP.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Range is an LSP text range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position is an LSP 0-based line/char position.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// CallHierarchyItem is an LSP call hierarchy node.
// Data uses json.RawMessage so the server-supplied opaque token round-trips
// byte-for-byte (the TS server stores position info there; a Go any{}
// round-trip corrupts it and causes -32603 errors in incomingCalls).
type CallHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"` // SymbolKind
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Detail         string          `json:"detail,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// IncomingCall is one entry in callHierarchy/incomingCalls.
type IncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// OutgoingCall is one entry in callHierarchy/outgoingCalls.
type OutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// PrepareCallHierarchy issues textDocument/prepareCallHierarchy at the given
// position. Returns nil if the server cannot prepare a hierarchy here.
func (c *Client) PrepareCallHierarchy(ctx context.Context, uri string, line, char int) ([]CallHierarchyItem, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result []CallHierarchyItem
	if err := c.call(ctx, "textDocument/prepareCallHierarchy", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// IncomingCalls fetches the incoming (caller) side of a call hierarchy item.
func (c *Client) IncomingCalls(ctx context.Context, item CallHierarchyItem) ([]IncomingCall, error) {
	var result []IncomingCall
	if err := c.call(ctx, "callHierarchy/incomingCalls", map[string]any{"item": item}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// OutgoingCalls fetches the outgoing (callee) side of a call hierarchy item.
func (c *Client) OutgoingCalls(ctx context.Context, item CallHierarchyItem) ([]OutgoingCall, error) {
	var result []OutgoingCall
	if err := c.call(ctx, "callHierarchy/outgoingCalls", map[string]any{"item": item}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Implementation issues textDocument/implementation at the given position.
func (c *Client) Implementation(ctx context.Context, uri string, line, char int) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result []Location
	if err := c.call(ctx, "textDocument/implementation", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// initialize sends the LSP initialize + initialized handshake.
func (c *Client) initialize(rootURI string) error {
	params := map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"callHierarchy": map[string]any{"dynamicRegistration": false},
				"implementation": map[string]any{
					"dynamicRegistration": false,
					"linkSupport":         false,
				},
			},
		},
		"initializationOptions": map[string]any{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.call(ctx, "initialize", params, nil); err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

// --- JSON-RPC 2.0 framing ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)
	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := c.send(req); err != nil {
		return err
	}
	// Read responses until we find ours (skip server→client notifications).
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("lsprecall: timeout waiting for %s response", method)
		}
		resp, err := c.readMessage()
		if err != nil {
			return fmt.Errorf("lsprecall: read response for %s: %w", method, err)
		}
		var r rpcResponse
		if err := json.Unmarshal(resp, &r); err != nil {
			continue // skip malformed messages (e.g. server stderr noise)
		}
		if r.ID == nil || *r.ID != id {
			continue // notification or different request — skip
		}
		if r.Error != nil {
			return fmt.Errorf("lsprecall: %s error %d: %s", method, r.Error.Code, r.Error.Message)
		}
		if result != nil && r.Result != nil {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	}
}

func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := fmt.Fprint(c.stdin, header); err != nil {
		return err
	}
	_, err = c.stdin.Write(data)
	return err
}

func (c *Client) readMessage() ([]byte, error) {
	var contentLength int
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line = end of headers
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
			contentLength = n
		}
	}
	if contentLength == 0 {
		return nil, fmt.Errorf("lsprecall: message with zero Content-Length")
	}
	buf := make([]byte, contentLength)
	if _, err := io.ReadFull(c.stdout, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
