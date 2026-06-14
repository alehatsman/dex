package mcp

import (
	"path/filepath"
	"testing"
)

// TestGoFuncHasHTTPHandlerSig locks issue #522: the http_handler detector must
// classify by signature, not by a handle/serve name prefix. Only a function
// that returns http.HandlerFunc/http.Handler or takes (http.ResponseWriter,
// *http.Request) is an HTTP handler; same-prefixed non-handlers are not.
func TestGoFuncHasHTTPHandlerSig(t *testing.T) {
	const src = `package app

import "net/http"

type Server struct{}
type Watcher struct{}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {}

func handleAsk() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {}
}

func makeRouter() http.Handler {
	return nil
}

func serveReadMode() bool { return true }

func ServerInstructions() string { return "" }

func (wt *Watcher) handle(ev int) {}

func handleWrapped(
	w http.ResponseWriter,
	r *http.Request,
) {
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "app.go")
	writeFile(t, path, src)

	// 1-based declaration lines within src (line 1 is "package app").
	cases := []struct {
		name      string
		startLine int
		want      bool
	}{
		{"handleStatus (w,r)", 8, true},
		{"handleAsk returns HandlerFunc", 10, true},
		{"makeRouter returns http.Handler", 14, true},
		{"serveReadMode returns bool", 18, false},
		{"ServerInstructions returns string", 20, false},
		{"Watcher.handle fsnotify", 22, false},
		{"handleWrapped multiline (w,r)", 24, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goFuncHasHTTPHandlerSig(path, c.startLine); got != c.want {
				t.Errorf("goFuncHasHTTPHandlerSig(%s:%d) = %v, want %v", "app.go", c.startLine, got, c.want)
			}
		})
	}

	// A missing file or non-positive line must not panic and must be false.
	if goFuncHasHTTPHandlerSig(filepath.Join(dir, "nope.go"), 1) {
		t.Error("missing file should yield false")
	}
	if goFuncHasHTTPHandlerSig(path, 0) {
		t.Error("non-positive startLine should yield false")
	}
}
