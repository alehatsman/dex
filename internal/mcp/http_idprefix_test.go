// Copyright 2026 Aleh Atsman
//
// Regression tests for #515: the boot banner prints a 12-char project-id
// prefix, so the REST/MCP routers must resolve an unambiguous prefix (not
// only the full 64-char id) or an agent pasting the banner id gets a hard
// "unknown project id" on the first try.

package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRegistryID(t *testing.T) {
	reg := map[string]string{
		"abc123def456789": "/root/alpha",
		"abc999aaa000111": "/root/beta",
		"xyz000111222333": "/root/gamma",
	}
	tests := []struct {
		name          string
		id            string
		wantCanonical string
		wantOK        bool
		wantAmbiguous bool
	}{
		{"exact full id", "xyz000111222333", "xyz000111222333", true, false},
		{"unique 12-char prefix", "xyz000111222", "xyz000111222333", true, false},
		{"unique short prefix", "x", "xyz000111222333", true, false},
		{"ambiguous prefix", "abc", "", false, true},
		{"ambiguous resolves once distinguished", "abc1", "abc123def456789", true, false},
		{"unknown prefix", "nope", "", false, false},
		{"exact wins over being a prefix of nothing else", "abc123def456789", "abc123def456789", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonical, ok, ambiguous := resolveRegistryID(tt.id, reg)
			if canonical != tt.wantCanonical || ok != tt.wantOK || ambiguous != tt.wantAmbiguous {
				t.Errorf("resolveRegistryID(%q) = (%q,%v,%v), want (%q,%v,%v)",
					tt.id, canonical, ok, ambiguous, tt.wantCanonical, tt.wantOK, tt.wantAmbiguous)
			}
		})
	}
}

// TestHTTPProjectIDPrefixResolves drives the full REST stack: an agent that
// copies the 12-char banner prefix into a call must reach the handler, not
// get a 404. End-to-end repro of #515.
func TestHTTPProjectIDPrefixResolves(t *testing.T) {
	srv := stubServer(t)
	dir := t.TempDir()
	registry, err := BuildProjectRegistry([]string{dir})
	if err != nil {
		t.Fatalf("BuildProjectRegistry: %v", err)
	}
	var fullID string
	for k := range registry {
		fullID = k
		break
	}
	if len(fullID) <= 12 {
		t.Fatalf("project id %q shorter than the 12-char banner prefix", fullID)
	}
	prefix := fullID[:12] // exactly what serve.go prints in the boot banner

	ts := startTestHTTPServer(t, srv, RunHTTPOptions{Projects: registry})
	resp, err := http.Post(ts.URL+"/v1/projects/"+prefix+"/query",
		"application/json", strings.NewReader(`{"input":"where is the watcher?"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (banner prefix should resolve, not 404)", resp.StatusCode)
	}
	var out map[string]any
	respBody, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, respBody)
	}
	// Prose routes to the semantic lane (result.semantic.project); the check
	// stays lenient (as it always has) since a fresh temp dir has no index.
	var proj string
	if result, ok := out["result"].(map[string]any); ok {
		if sem, ok := result["semantic"].(map[string]any); ok {
			proj, _ = sem["project"].(string)
		}
	}
	if proj != "" {
		dirReal, _ := filepath.EvalSymlinks(dir)
		if proj != dirReal && proj != dir {
			t.Errorf("prefix resolved project=%q, want %q", proj, dirReal)
		}
	}
}
