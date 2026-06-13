package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/alehatsman/dex/internal/graph"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	st, err := store.OpenWith(ctx, p.DBPath, s.StoreOpts)
	if err != nil {
		return nil, RoutesOutput{Status: "error", Hint: fmt.Sprintf("open index: %v", err)}, nil
	}
	defer func() { _ = st.Close() }()

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

	add := func(n graphNode, kind, registeredBy string) {
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
	for _, n := range view.nodesByName["ServeHTTP"] {
		if n.Kind == graph.NodeMethod {
			add(n, "http_handler", "")
		}
	}

	// 2. Functions/methods named handle*/Handle* (excluding ServeHTTP already captured).
	for name, nodes := range view.nodesByName {
		lower := strings.ToLower(name)
		if name == "ServeHTTP" {
			continue
		}
		if strings.HasPrefix(lower, "handle") || strings.HasPrefix(lower, "serve") {
			for _, n := range nodes {
				if n.Kind == graph.NodeFunction || n.Kind == graph.NodeMethod {
					add(n, "http_handler", "")
				}
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
		for _, regNode := range view.nodesByName[regName] {
			for _, e := range view.edgesByDst[regNode.ID] {
				if e.Kind != graph.EdgeCalls {
					continue
				}
				caller, ok := view.nodesByID[e.SrcID]
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
