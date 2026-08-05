package health

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alehatsman/dex/internal/backendhttp"
	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
)

// TestClassifyDeep pins the readiness classification matrix, including the
// embed-is-critical / chat-rerank-degrade split and the cold-timeout carve-out.
func TestClassifyDeep(t *testing.T) {
	embedP := Probe{Name: "embed", URL: "http://e", Model: "m-embed"}
	chatP := Probe{Name: "chat", URL: "http://c", Model: "m-chat"}

	t.Run("usable", func(t *testing.T) {
		c := ClassifyDeep(embedP, nil)
		if c.Status != OK {
			t.Fatalf("status=%v, want OK", c.Status)
		}
		if !strings.Contains(c.Detail, "usable") {
			t.Errorf("detail=%q, want it to say usable", c.Detail)
		}
	})

	t.Run("cold timeout is a non-critical warning", func(t *testing.T) {
		c := ClassifyDeep(embedP, context.DeadlineExceeded)
		if c.Status != Warn || c.Critical {
			t.Fatalf("status=%v critical=%v, want Warn non-critical", c.Status, c.Critical)
		}
		if !strings.Contains(c.Detail, "cold") {
			t.Errorf("detail=%q, want a cold-load hint", c.Detail)
		}
	})

	t.Run("embed unreachable is critical", func(t *testing.T) {
		c := ClassifyDeep(embedP, fmt.Errorf("%w: dial tcp", embed.ErrUnreachable))
		if c.Status != Fail || !c.Critical {
			t.Fatalf("status=%v critical=%v, want Fail critical", c.Status, c.Critical)
		}
		if !strings.Contains(c.Detail, "UNREACHABLE") {
			t.Errorf("detail=%q, want UNREACHABLE", c.Detail)
		}
	})

	t.Run("chat unreachable degrades (warn)", func(t *testing.T) {
		c := ClassifyDeep(chatP, fmt.Errorf("%w: dial tcp", chat.ErrUnreachable))
		if c.Status != Warn || c.Critical {
			t.Fatalf("status=%v critical=%v, want Warn non-critical", c.Status, c.Critical)
		}
	})

	t.Run("overloaded (429) is reachable-but-busy, not UNREACHABLE", func(t *testing.T) {
		rerankP := Probe{Name: "rerank", URL: "http://r", Model: "m-rerank"}
		// The rerank client wraps 429/5xx as ErrUnreachable but keeps the
		// "http <code>:" marker — deep mode must read the code, not the sentinel.
		c := ClassifyDeep(rerankP, fmt.Errorf("%w: %w", rerank.ErrUnreachable, &backendhttp.StatusError{Code: 429, Body: "model overloaded"}))
		if c.Status != Warn || c.Critical {
			t.Fatalf("status=%v critical=%v, want Warn non-critical", c.Status, c.Critical)
		}
		if strings.Contains(c.Detail, "UNREACHABLE") || !strings.Contains(c.Detail, "overloaded") {
			t.Errorf("detail=%q, want an overloaded/busy message, not UNREACHABLE", c.Detail)
		}
	})

	t.Run("model not served -> not ready with targeted hint", func(t *testing.T) {
		c := ClassifyDeep(embedP, fmt.Errorf("embed: %w", &backendhttp.StatusError{Code: 404, Body: "model not found"}))
		if c.Status != Fail || !c.Critical {
			t.Fatalf("status=%v critical=%v, want Fail critical", c.Status, c.Critical)
		}
		if len(c.Hints) == 0 || !strings.Contains(strings.Join(c.Hints, " "), "m-embed") {
			t.Errorf("hints=%v, want one naming the model", c.Hints)
		}
	})
}
