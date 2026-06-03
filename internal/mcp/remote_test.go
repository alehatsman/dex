package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testProjectID = "abc123def456"

// recordingServer captures the last request the shim made and replies with a
// caller-supplied status + body. It mirrors the relevant `dex serve` routes
// closely enough to assert the proxy's method/path/auth/body behavior.
type recordingServer struct {
	srv       *httptest.Server
	gotMethod string
	gotPath   string
	gotAuth   string
	gotCT     string
	gotBody   []byte
	replyCode int
	replyBody string
}

func newRecordingServer() *recordingServer {
	rs := &recordingServer{replyCode: http.StatusOK, replyBody: "{}"}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.gotMethod = r.Method
		rs.gotPath = r.URL.Path
		rs.gotAuth = r.Header.Get("Authorization")
		rs.gotCT = r.Header.Get("Content-Type")
		if r.Body != nil {
			rs.gotBody, _ = readAll(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rs.replyCode)
		_, _ = w.Write([]byte(rs.replyBody))
	}))
	return rs
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func (rs *recordingServer) close() { rs.srv.Close() }

func (rs *recordingServer) client(token string) *remoteClient {
	return &remoteClient{
		base:      rs.srv.URL,
		token:     token,
		projectID: testProjectID,
		http:      rs.srv.Client(),
	}
}

// TestRemoteProxyRequestShape verifies every tool maps to the correct REST
// route, carries the bound project id, sends the bearer token, and uses the
// right HTTP method.
func TestRemoteProxyRequestShape(t *testing.T) {
	base := "/v1/projects/" + testProjectID
	cases := []struct {
		name       string
		call       func(ctx context.Context, rc *remoteClient) error
		wantMethod string
		wantPath   string
	}{
		{"ask", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.contextRouter(c, nil, ContextInput{Question: "q"})
			return err
		}, http.MethodPost, base + "/ask"},
		{"search", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.search(c, nil, SearchInput{})
			return err
		}, http.MethodPost, base + "/search/semantic"},
		{"findSymbol", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.findSymbol(c, nil, FindSymbolInput{})
			return err
		}, http.MethodPost, base + "/search/symbol"},
		{"related", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.related(c, nil, RelatedInput{})
			return err
		}, http.MethodPost, base + "/graph/neighbors"},
		{"graphDeps", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphDeps(c, nil, GraphDepsInput{})
			return err
		}, http.MethodPost, base + "/graph/deps"},
		{"graphCallers", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphCallers(c, nil, CallEdgeInput{})
			return err
		}, http.MethodPost, base + "/graph/callers"},
		{"graphCallees", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphCallees(c, nil, CallEdgeInput{})
			return err
		}, http.MethodPost, base + "/graph/callees"},
		{"graphLinks", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphLinks(c, nil, DocLinkInput{})
			return err
		}, http.MethodPost, base + "/graph/links"},
		{"graphBacklinks", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphBacklinks(c, nil, DocLinkInput{})
			return err
		}, http.MethodPost, base + "/graph/backlinks"},
		{"graphTags", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.graphTags(c, nil, TagInput{})
			return err
		}, http.MethodPost, base + "/graph/tags"},
		{"summarize", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.summarize(c, nil, SummarizeInput{})
			return err
		}, http.MethodPost, base + "/view/summarize"},
		{"status", func(c context.Context, rc *remoteClient) error {
			_, _, err := rc.status(c, nil, StatusInput{})
			return err
		}, http.MethodGet, "/v1/status"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRecordingServer()
			defer rs.close()
			rc := rs.client("tok-123")

			if err := tc.call(context.Background(), rc); err != nil {
				t.Fatalf("call: %v", err)
			}
			if rs.gotMethod != tc.wantMethod {
				t.Errorf("method = %q, want %q", rs.gotMethod, tc.wantMethod)
			}
			if rs.gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", rs.gotPath, tc.wantPath)
			}
			if rs.gotAuth != "Bearer tok-123" {
				t.Errorf("auth = %q, want %q", rs.gotAuth, "Bearer tok-123")
			}
			if tc.wantMethod == http.MethodPost && rs.gotCT != "application/json" {
				t.Errorf("content-type = %q, want application/json", rs.gotCT)
			}
		})
	}
}

// TestRemoteProxyDecodesOutput confirms the proxy decodes the daemon's JSON
// response into the shared Output struct unchanged.
func TestRemoteProxyDecodesOutput(t *testing.T) {
	rs := newRecordingServer()
	defer rs.close()
	rs.replyBody = `{"status":"ok","answer":"the answer","answer_model":"qwen2.5-coder:14b","intent":"architecture"}`
	rc := rs.client("tok")

	_, out, err := rc.contextRouter(context.Background(), nil, ContextInput{Question: "how does indexing work?"})
	if err != nil {
		t.Fatalf("contextRouter: %v", err)
	}
	if out.Status != "ok" || out.Answer != "the answer" || out.AnswerModel != "qwen2.5-coder:14b" || out.Intent != "architecture" {
		t.Fatalf("decoded output mismatch: %+v", out)
	}

	// The request body should carry the question (server overrides project, so
	// we only assert what the client sends).
	var sent ContextInput
	if err := json.Unmarshal(rs.gotBody, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	if sent.Question != "how does indexing work?" {
		t.Errorf("sent question = %q", sent.Question)
	}
}

// TestRemoteProxyPassesThroughDegradation verifies a 200 carrying a non-ok
// Status (no-index etc.) flows through as a normal result, not an error —
// matching local handler behavior.
func TestRemoteProxyPassesThroughDegradation(t *testing.T) {
	rs := newRecordingServer()
	defer rs.close()
	rs.replyBody = `{"status":"no-index","hint":"run dex index first"}`
	rc := rs.client("tok")

	_, out, err := rc.contextRouter(context.Background(), nil, ContextInput{Question: "q"})
	if err != nil {
		t.Fatalf("expected nil error for 200 degradation, got %v", err)
	}
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
}

// TestRemoteProxyHTTPError verifies a non-2xx response surfaces as an error
// carrying the daemon's {"error"} message.
func TestRemoteProxyHTTPError(t *testing.T) {
	rs := newRecordingServer()
	defer rs.close()
	rs.replyCode = http.StatusNotFound
	rs.replyBody = `{"error":"unknown project id: abc123def456"}`
	rc := rs.client("tok")

	_, _, err := rc.contextRouter(context.Background(), nil, ContextInput{Question: "q"})
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "unknown project id") {
		t.Errorf("error = %q, want it to mention the daemon message", err)
	}
}

// TestRemoteProxyNoToken confirms no Authorization header is sent when the
// token is empty (loopback / token-less daemon).
func TestRemoteProxyNoToken(t *testing.T) {
	rs := newRecordingServer()
	defer rs.close()
	rc := rs.client("")

	if _, _, err := rc.status(context.Background(), nil, StatusInput{}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if rs.gotAuth != "" {
		t.Errorf("auth = %q, want empty", rs.gotAuth)
	}
}

// TestListRemoteProjects exercises the discovery path used to resolve a
// project id when the operator didn't pass one.
func TestListRemoteProjects(t *testing.T) {
	rs := newRecordingServer()
	defer rs.close()
	rs.replyBody = `{"projects":[{"id":"id-a","root":"/srv/a"},{"id":"id-b","root":"/srv/b"}]}`

	projects, err := ListRemoteProjects(context.Background(), rs.srv.URL, "tok", rs.srv.Client())
	if err != nil {
		t.Fatalf("ListRemoteProjects: %v", err)
	}
	if rs.gotPath != "/v1/projects" || rs.gotMethod != http.MethodGet {
		t.Errorf("hit %s %s, want GET /v1/projects", rs.gotMethod, rs.gotPath)
	}
	if rs.gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", rs.gotAuth)
	}
	if len(projects) != 2 || projects[0].ID != "id-a" || projects[1].Root != "/srv/b" {
		t.Fatalf("projects = %+v", projects)
	}
}
