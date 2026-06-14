package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/graphquery"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// goFuncHasHTTPHandlerSig reports whether the Go function/method whose
// declaration begins at path:startLine has an HTTP-handler signature: it
// returns http.HandlerFunc / http.Handler, or takes (http.ResponseWriter,
// *http.Request). It reads only the declaration (up to the body's opening
// brace), so it is cheap. A read error yields false — the name heuristic
// alone is not enough to claim a route (#522).
func goFuncHasHTTPHandlerSig(absPath string, startLine int) bool {
	if startLine <= 0 {
		return false
	}
	f, err := os.Open(absPath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var decl strings.Builder
	line := 0
	for sc.Scan() {
		line++
		if line < startLine {
			continue
		}
		text := sc.Text()
		decl.WriteString(text)
		decl.WriteByte('\n')
		// The signature is complete once the body opens; cap the span so a
		// malformed/odd node can't make us read the whole file.
		if strings.Contains(text, "{") || line-startLine >= 12 {
			break
		}
	}

	d := decl.String()
	// http.Handler also matches the http.HandlerFunc return type. Within the
	// name-prefixed candidate set this is a strong handler signal; a bare
	// "http.Handler" param on a non-handle/serve function never reaches here.
	if strings.Contains(d, "http.Handler") {
		return true
	}
	return strings.Contains(d, "http.ResponseWriter") && strings.Contains(d, "http.Request")
}

type RoutesInput struct {
	ProjectRoot string `json:"project_root,omitempty" jsonschema:"absolute path to the project root; defaults to the server's working directory"`
}

// RouteEntry is one detected handler or registration site.
type RouteEntry struct {
	QualifiedName string `json:"qualified_name"`
	Kind          string `json:"kind"` // "http_handler" | "mcp_tool" | "grpc_handler" | "registration"
	Path          string `json:"path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	RegisteredBy  string `json:"registered_by,omitempty"` // caller that registers this handler
}

type RoutesOutput struct {
	Status  string       `json:"status"` // "ok" | "no-index" | "no-graph" | "error"
	Hint    string       `json:"hint,omitempty"`
	Project string       `json:"project,omitempty"`
	Routes  []RouteEntry `json:"routes,omitempty"`
	Total   int          `json:"total"`
}

func (s *Server) Routes(ctx context.Context, in RoutesInput) (RoutesOutput, error) {
	_, out, err := s.routes(ctx, nil, in)
	return out, err
}

func (s *Server) routes(ctx context.Context, _ *sdk.CallToolRequest, in RoutesInput) (*sdk.CallToolResult, RoutesOutput, error) {
	p, hint := s.resolveProject(in.ProjectRoot)
	if hint != "" {
		return nil, RoutesOutput{Status: "error", Hint: hint}, nil
	}
	if _, err := os.Stat(p.DBPath); errors.Is(err, os.ErrNotExist) {
		return nil, RoutesOutput{Status: "no-index", Project: p.Root,
			Hint: fmt.Sprintf("no index for %s — run `dex index %s` first.", p.Root, p.Root)}, nil
	}
	st, err := s.openStore(p.DBPath)
	if err != nil {
		return nil, RoutesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}

	view, err := s.cachedLoadGraphView(ctx, st, p.DBPath)
	if err != nil {
		return nil, RoutesOutput{Status: "error", Hint: fmt.Sprintf("load graph: %v", err)}, nil
	}
	if view == nil {
		return nil, RoutesOutput{Status: "no-graph", Project: p.Root,
			Hint: fmt.Sprintf("graph not indexed for %s — run `dex index %s --graph=only`.", p.Root, p.Root)}, nil
	}

	var routes []RouteEntry
	seen := map[string]bool{}

	add := func(n graphquery.Node, kind, registeredBy string) {
		key := n.ID + "|" + kind
		if seen[key] {
			return
		}
		seen[key] = true
		routes = append(routes, RouteEntry{
			QualifiedName: n.QualifiedName,
			Kind:          kind,
			Path:          n.FilePath,
			StartLine:     n.StartLine,
			EndLine:       n.EndLine,
			RegisteredBy:  registeredBy,
		})
	}

	// 1. ServeHTTP implementations → HTTP handlers.
	for _, n := range view.NodesByName["ServeHTTP"] {
		if n.Kind == graph.NodeMethod {
			add(n, "http_handler", "")
		}
	}

	// 2. Functions/methods named handle*/Handle* (excluding ServeHTTP already
	//    captured). The name alone over-matches — serverReadMode, a Watcher's
	//    fsnotify handle(), ServerInstructions all share a handle/serve prefix
	//    yet are not HTTP handlers (#522). For Go, corroborate with the
	//    signature: a real handler returns http.HandlerFunc/http.Handler or
	//    takes (http.ResponseWriter, *http.Request). Other languages keep the
	//    name signal (no cheap signature check here).
	for name, nodes := range view.NodesByName {
		lower := strings.ToLower(name)
		if name == "ServeHTTP" {
			continue
		}
		if strings.HasPrefix(lower, "handle") || strings.HasPrefix(lower, "serve") {
			for _, n := range nodes {
				if n.Kind != graph.NodeFunction && n.Kind != graph.NodeMethod {
					continue
				}
				if strings.HasSuffix(n.FilePath, ".go") &&
					!goFuncHasHTTPHandlerSig(filepath.Join(p.Root, n.FilePath), n.StartLine) {
					continue
				}
				add(n, "http_handler", "")
			}
		}
	}

	// 3. Callers of registration functions: AddTool → mcp_tool,
	//    Handle/HandleFunc → registration, RegisterService → grpc_handler.
	registrationTargets := map[string]string{
		"AddTool":         "mcp_tool",
		"Handle":          "registration",
		"HandleFunc":      "registration",
		"HandleMethod":    "registration",
		"GET":             "registration",
		"POST":            "registration",
		"PUT":             "registration",
		"DELETE":          "registration",
		"PATCH":           "registration",
		"RegisterService": "grpc_handler",
	}
	for regName, kind := range registrationTargets {
		for _, regNode := range view.NodesByName[regName] {
			for _, e := range view.EdgesByDst[regNode.ID] {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				caller, ok := view.NodesByID[e.SrcID]
				if !ok {
					continue
				}
				add(caller, kind, regNode.QualifiedName)
			}
		}
	}

	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.StartLine < b.StartLine
	})

	out := RoutesOutput{Status: "ok", Project: p.Root, Routes: routes, Total: len(routes)}
	if len(routes) == 0 {
		out.Hint = "no handler patterns detected — the graph may not be indexed yet (`dex index . --graph=only`) or this project uses an unrecognised routing library."
	}
	return nil, out, nil
}
