package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// oneProjectRegistry builds a single-project registry rooted at a temp dir
// (no index on disk — tool calls return status "no-index", which is enough to
// prove the transport, scoping, and auth wiring without a live embed/chat
// backend). Returns the id and resolved root.
func oneProjectRegistry(t *testing.T) (id, root string, projects map[string]string) {
	t.Helper()
	dir := t.TempDir()
	projects, err := BuildProjectRegistry([]string{dir})
	if err != nil {
		t.Fatalf("BuildProjectRegistry: %v", err)
	}
	for id, root = range projects { // exactly one entry — grab it
		break
	}
	return id, root, projects
}

// mcpConnect dials the streamable-HTTP MCP endpoint at baseURL+path using the
// given http client, returning a connected session.
func mcpConnect(t *testing.T, ctx context.Context, baseURL, path string, hc *http.Client) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             baseURL + path,
		HTTPClient:           hc,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", path, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestHTTPMCPSession is the end-to-end transport test: a real MCP client
// initializes over streamable HTTP, lists tools, and calls `query`. It asserts
// the session works and that the per-project scoping injected the bound root
// (the tool resolves the registry project, not the daemon default).
func TestHTTPMCPSession(t *testing.T) {
	ctx := context.Background()
	srv := stubServer(t)
	id, root, projects := oneProjectRegistry(t)
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: projects})

	cs := mcpConnect(t, ctx, ts.URL, "/v1/projects/"+id+"/mcp", ts.Client())

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var hasQuery bool
	for _, tool := range tools.Tools {
		if tool.Name == "query" {
			hasQuery = true
		}
	}
	if !hasQuery {
		t.Fatalf("query tool not advertised; got %d tools", len(tools.Tools))
	}

	// A prose input routes query to the semantic lane (contextRouter); the pack
	// is wrapped under result.ask in the query envelope.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{
		Name:      "query",
		Arguments: map[string]any{"input": "anything at all as a question"},
	})
	if err != nil {
		t.Fatalf("CallTool query: %v", err)
	}

	var out QueryOutput
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content: %v (raw=%s)", err, raw)
	}
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index (empty index)", out.Status)
	}
	if out.Result.Semantic == nil {
		t.Fatalf("prose input should populate result.semantic, got %+v", out.Result)
	}
	// Scoping proof: the handler stamped the registry root onto the Input, so
	// the tool resolved the bound project rather than the server default.
	if out.Result.Semantic.Project != root {
		t.Errorf("project = %q, want bound root %q", out.Result.Semantic.Project, root)
	}
}

// TestHTTPMCPUnknownProject confirms an unknown {id} is refused (getServer
// returns nil => 400), so the session fails to establish.
func TestHTTPMCPUnknownProject(t *testing.T) {
	ctx := context.Background()
	srv := stubServer(t)
	_, _, projects := oneProjectRegistry(t)
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: projects})

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   ts.URL + "/v1/projects/deadbeef/mcp",
		HTTPClient: ts.Client(),
	}, nil)
	if err == nil {
		_ = cs.Close()
		t.Fatal("expected connect to fail for unknown project id")
	}
}

// bearerRoundTripper injects an Authorization header so the SDK client can
// authenticate against a token-protected daemon (the transport has no direct
// header hook).
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// TestHTTPMCPAuth confirms the MCP endpoint sits behind the same bearer auth
// as the REST surface: no token fails, the right token succeeds.
func TestHTTPMCPAuth(t *testing.T) {
	ctx := context.Background()
	srv := stubServer(t)
	id, _, projects := oneProjectRegistry(t)
	const token = "s3cret"
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: projects, Token: token})
	path := "/v1/projects/" + id + "/mcp"

	// No token → connect must fail (401 before the session initializes).
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	if cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   ts.URL + path,
		HTTPClient: ts.Client(),
	}, nil); err == nil {
		_ = cs.Close()
		t.Fatal("expected connect to fail without bearer token")
	}

	// Correct token → session establishes and lists tools.
	authed := &http.Client{Transport: bearerRoundTripper{token: token, base: ts.Client().Transport}}
	cs := mcpConnect(t, ctx, ts.URL, path, authed)
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools with token: %v", err)
	}
}
