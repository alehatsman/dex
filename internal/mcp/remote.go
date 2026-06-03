package mcp

// Remote stdio<->REST shim — the `dex mcp --remote` mode. A client (e.g. an
// in-container `claude`) attaches dex over MCP stdio as usual, but this
// process holds no index, embeddings, or GPU: every tool call is forwarded
// to a remote `dex serve` daemon's REST surface (internal/mcp/http.go),
// scoped to a single project id and authenticated with a bearer token.
//
// The shim reuses the exact Input/Output structs the local handlers use and
// registers tools through the same registerTools path, so the MCP tool
// names, JSON schemas, and response shapes are byte-identical to a local
// stdio server — the client cannot tell whether the index is local or
// remote. This is #6 Option A: the stopgap until `dex serve` speaks
// streamable-HTTP MCP natively (#49 / Option B), which retires this shim.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// RemoteOptions configures RunStdioRemote.
type RemoteOptions struct {
	// BaseURL is the dex serve root, e.g. "http://host:8080". The /v1
	// prefix is appended by the shim; a trailing slash is tolerated.
	BaseURL string
	// Token is the bearer token (DEX_SERVE_TOKEN). Empty is valid only when
	// the remote daemon is itself token-less (loopback-bound).
	Token string
	// ProjectID is the dex project id (sha256 of the canonical host root)
	// every tool call is bound to. Must match an id in the remote's
	// registry; the shim cannot reach any other project.
	ProjectID string
	// HTTP is the client used for proxy calls. Nil installs a default with a
	// generous timeout (the `ask` leg can run an embed + chat synthesis).
	HTTP *http.Client
}

// RemoteProject is one entry from a remote daemon's project registry
// (GET /v1/projects).
type RemoteProject struct {
	ID   string `json:"id"`
	Root string `json:"root"`
}

// remoteClient implements toolSurface by proxying each tool call to the REST
// endpoints `dex serve` exposes. It owns no index — every handler is a thin
// HTTP request that marshals the shared Input struct and decodes the shared
// Output struct.
type remoteClient struct {
	base      string // BaseURL, trailing slash trimmed
	token     string
	projectID string
	http      *http.Client
}

// RunStdioRemote runs an MCP server on stdio whose tool handlers proxy to a
// remote `dex serve` REST endpoint. Blocks until ctx is cancelled or the
// transport closes.
func RunStdioRemote(ctx context.Context, opts RemoteOptions) error {
	httpc := opts.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 120 * time.Second}
	}
	rc := &remoteClient{
		base:      strings.TrimRight(opts.BaseURL, "/"),
		token:     opts.Token,
		projectID: opts.ProjectID,
		http:      httpc,
	}

	srv := sdk.NewServer(&sdk.Implementation{Name: "dex", Version: Version}, nil)

	// The shim can't see the remote's chat wiring, so it registers
	// view_summarize whenever raw tools are on (DEX_EXPOSE_RAW_TOOLS). If the
	// remote has no chat client the /view/summarize endpoint returns
	// 'chat-service-unreachable' — the same degradation a local server
	// reports — so over-registering is harmless.
	raw := exposeRawTools()
	registerTools(srv, rc, raw, raw)

	return srv.Run(ctx, &sdk.StdioTransport{})
}

// ListRemoteProjects fetches the remote daemon's project registry. Used by
// `dex mcp --remote` to resolve a project id when the operator didn't pass
// one (and to list the choices in the error when the id is ambiguous).
func ListRemoteProjects(ctx context.Context, baseURL, token string, httpc *http.Client) ([]RemoteProject, error) {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	rc := &remoteClient{base: strings.TrimRight(baseURL, "/"), token: token, http: httpc}
	var resp struct {
		Projects []RemoteProject `json:"projects"`
	}
	if err := rc.do(ctx, http.MethodGet, rc.base+"/v1/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// projectPath builds a /v1/projects/{id}/... URL for the bound project.
func (rc *remoteClient) projectPath(suffix string) string {
	return rc.base + "/v1/projects/" + rc.projectID + suffix
}

// do issues an HTTP request to url, optionally sending in as a JSON body and
// decoding a JSON response into out (either may be nil). A non-2xx response
// is surfaced as an error carrying the server's {"error"} message — a
// transport/auth/routing failure must not be silently swallowed as an empty
// result. Per-tool degradations (no-index, no-graph, …) come back as 200 +
// a Status field on out and pass through unchanged, matching local behavior.
func (rc *remoteClient) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rc.token != "" {
		req.Header.Set("Authorization", "Bearer "+rc.token)
	}

	resp, err := rc.http.Do(req)
	if err != nil {
		return fmt.Errorf("dex serve %s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteHTTPError(method, url, resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, url, err)
	}
	return nil
}

// remoteHTTPError turns a non-2xx response into an error, preferring the
// daemon's JSON {"error":"…"} payload over the raw body.
func remoteHTTPError(method, url string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("dex serve %s %s: %s: %s", method, url, resp.Status, e.Error)
	}
	if msg := strings.TrimSpace(string(b)); msg != "" {
		return fmt.Errorf("dex serve %s %s: %s: %s", method, url, resp.Status, msg)
	}
	return fmt.Errorf("dex serve %s %s: %s", method, url, resp.Status)
}

// ─── toolSurface implementation (proxy handlers) ────────────────────────────
//
// Each handler returns a nil *sdk.CallToolResult plus the decoded Output; the
// SDK marshals the structured result, exactly as it does for the local
// handlers. The REST routes mirror buildHTTPHandler in http.go.

func (rc *remoteClient) contextRouter(ctx context.Context, _ *sdk.CallToolRequest, in ContextInput) (*sdk.CallToolResult, ContextOutput, error) {
	var out ContextOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/ask"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) search(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
	var out SearchOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/search/semantic"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) findSymbol(ctx context.Context, _ *sdk.CallToolRequest, in FindSymbolInput) (*sdk.CallToolResult, FindSymbolOutput, error) {
	var out FindSymbolOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/search/symbol"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) related(ctx context.Context, _ *sdk.CallToolRequest, in RelatedInput) (*sdk.CallToolResult, RelatedOutput, error) {
	var out RelatedOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/neighbors"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphDeps(ctx context.Context, _ *sdk.CallToolRequest, in GraphDepsInput) (*sdk.CallToolResult, GraphDepsOutput, error) {
	var out GraphDepsOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/deps"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphCallers(ctx context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	var out CallEdgeOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/callers"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphCallees(ctx context.Context, _ *sdk.CallToolRequest, in CallEdgeInput) (*sdk.CallToolResult, CallEdgeOutput, error) {
	var out CallEdgeOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/callees"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphLinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	var out DocLinkOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/links"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphBacklinks(ctx context.Context, _ *sdk.CallToolRequest, in DocLinkInput) (*sdk.CallToolResult, DocLinkOutput, error) {
	var out DocLinkOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/backlinks"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) graphTags(ctx context.Context, _ *sdk.CallToolRequest, in TagInput) (*sdk.CallToolResult, TagOutput, error) {
	var out TagOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/graph/tags"), in, &out)
	return nil, out, err
}

func (rc *remoteClient) summarize(ctx context.Context, _ *sdk.CallToolRequest, in SummarizeInput) (*sdk.CallToolResult, SummarizeOutput, error) {
	var out SummarizeOutput
	err := rc.do(ctx, http.MethodPost, rc.projectPath("/view/summarize"), in, &out)
	return nil, out, err
}

// status maps to the daemon-global GET /v1/status (not project-scoped), so it
// ignores the bound project id and sends no body — mirroring handleStatus.
func (rc *remoteClient) status(ctx context.Context, _ *sdk.CallToolRequest, _ StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	var out StatusOutput
	err := rc.do(ctx, http.MethodGet, rc.base+"/v1/status", nil, &out)
	return nil, out, err
}
