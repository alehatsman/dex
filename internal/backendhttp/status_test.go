package backendhttp

import (
	"errors"
	"fmt"
	"testing"
)

func TestStatusErrorMessage(t *testing.T) {
	if got := (&StatusError{Code: 404, Body: "model not found"}).Error(); got != "http 404: model not found" {
		t.Errorf("with body: %q", got)
	}
	if got := (&StatusError{Code: 503}).Error(); got != "http 503" {
		t.Errorf("no body: %q", got)
	}
}

func TestStatusErrorRetryable(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{429, true}, {500, true}, {503, true}, {400, false}, {404, false}, {200, false}} {
		if got := (&StatusError{Code: tc.code}).Retryable(); got != tc.want {
			t.Errorf("Retryable(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// TestStatusErrorAsThroughWrap verifies the code is recoverable via errors.As
// even when composed alongside a sentinel with %w (the rerank breaker pattern).
func TestStatusErrorAsThroughWrap(t *testing.T) {
	sentinel := errors.New("service unreachable")
	err := fmt.Errorf("%w: %w", sentinel, &StatusError{Code: 429, Body: "overloaded"})

	if !errors.Is(err, sentinel) {
		t.Error("errors.Is(sentinel) = false, want true (breaker check must survive)")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 429 {
		t.Errorf("errors.As did not recover StatusError{429}; got %v", se)
	}
}
