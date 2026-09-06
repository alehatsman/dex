package mcp

// Shared handler plumbing for the REST transport mounted by
// Server.RunHTTP/buildHTTPHandler (http.go) — the endpoint layout, auth
// model, and project-registry semantics are documented once there; this file
// holds the two hand-written handlers (list-projects, status) and the
// jsonHandler[In,Out] generic every other route delegates to.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ─── shared handler helpers ─────────────────────────────────────────────────

// resolveProjectFromURL looks up the {id} path segment against the
// operator's registry. Writes the appropriate error response and
// returns "" when not found.
func resolveProjectFromURL(w http.ResponseWriter, r *http.Request, projects map[string]string) string {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return ""
	}
	canonical, ok, ambiguous := resolveRegistryID(id, projects)
	if ambiguous {
		writeError(w, http.StatusBadRequest, "ambiguous project id prefix: "+id)
		return ""
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown project id: "+id)
		return ""
	}
	return projects[canonical]
}

// resolveRegistryID maps a URL {id} to a canonical registry key, accepting
// either the full key or an unambiguous prefix of it. The boot banner
// prints a 12-char id prefix (serve.go), so an operator can paste that
// straight into a REST/MCP-over-HTTP call. An exact match always wins; a
// prefix that matches exactly one key resolves to it; a prefix matching
// more than one key is reported ambiguous (the caller turns that into an
// error rather than silently picking one).
func resolveRegistryID[V any](id string, registry map[string]V) (canonical string, ok bool, ambiguous bool) {
	if _, exact := registry[id]; exact {
		return id, true, false
	}
	var match string
	n := 0
	for k := range registry {
		if strings.HasPrefix(k, id) {
			match, n = k, n+1
		}
	}
	switch n {
	case 1:
		return match, true, false
	case 0:
		return "", false, false
	default:
		return "", false, true
	}
}

// decodeBody reads the request body into v. Empty bodies (Content-
// Length 0, no body) are accepted — most Inputs have only optional
// fields beyond the URL-supplied project id.
func decodeBody(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)) // 1 MiB cap
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// writeJSON serializes v as the response body with the given status.
// Errors during encoding are logged via the recorded status; we
// can't recover the wire state cleanly past WriteHeader.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleListProjects(projects map[string]string) http.HandlerFunc {
	type entry struct {
		ID   string `json:"id"`
		Root string `json:"root"`
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		out := make([]entry, 0, len(projects))
		for id, root := range projects {
			out = append(out, entry{ID: id, Root: root})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
		writeJSON(w, http.StatusOK, map[string]any{"projects": out})
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out, err := s.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// jsonHandler builds the HTTP handler shared by every project-scoped tool:
// resolve {id} to a project root, decode the JSON body into In, override its
// project field via bind (so a client can't smuggle a different path), invoke
// call, and encode the result. The handlers below differ only in their input
// type and service method, so they collapse to thin delegators.
func jsonHandler[In, Out any](
	projects map[string]string,
	bind func(in *In, root string),
	call func(ctx context.Context, in In) (Out, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := resolveProjectFromURL(w, r, projects)
		if root == "" {
			return
		}
		var in In
		if err := decodeBody(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		bind(&in, root)
		out, err := call(r.Context(), in)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
