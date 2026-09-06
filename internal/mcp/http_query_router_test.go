package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestRESTQueryRouterAccuracy is the REST-body variant of the router-accuracy
// gate (spec Validation, #851 — mirrors #849's CLI-flag variant,
// TestCLIQueryRouterAccuracy in cmd/dex): the same shape→lane corpus
// (routerShapeCorpus) routed through a real POST /v1/projects/{id}/query
// body, over the actual HTTP handler, asserting route.lane end to end. No
// index is needed — route classification happens before any lane handler
// runs (queryVerb → dispatchSingle → classifyQuery, then the lane dispatch),
// so a bare temp project root is enough.
func TestRESTQueryRouterAccuracy(t *testing.T) {
	srv := stubServer(t)
	dir := t.TempDir()
	registry, err := BuildProjectRegistry([]string{dir})
	if err != nil {
		t.Fatalf("BuildProjectRegistry: %v", err)
	}
	var id string
	for k := range registry {
		id = k
	}
	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: registry})

	for _, c := range routerShapeCorpus {
		t.Run(c.rung, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"input": c.input, "kind": c.kind})
			resp, err := http.Post(ts.URL+"/v1/projects/"+id+"/query", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /query: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			var out QueryOutput
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// route.lane names the populated result field, not the coarse
			// lane classifyQuery/semanticLane started from: symbol→trace
			// (dispatchExact) and the review intent→its own "review" lane
			// (semanticLane) both refine "semantic"/"symbol" further.
			want := c.wantLane
			switch {
			case want == "symbol":
				want = "trace"
			case c.kind == "review" || (c.kind == "" && want == "semantic" && c.rung == "review-prose"):
				want = "review"
			}
			if out.Route.Lane != want {
				t.Errorf("input=%q kind=%q -> route.lane=%q, want %q (detected=%q)",
					c.input, c.kind, out.Route.Lane, want, out.Route.Detected)
			}
		})
	}
}
