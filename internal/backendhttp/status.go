// Package backendhttp holds error types shared by the embed, chat, and rerank
// HTTP clients. It is a dependency-free leaf so every client can wrap non-2xx
// responses in one typed error without cross-importing each other.
package backendhttp

import "fmt"

// StatusError is a backend's non-2xx HTTP response. Clients wrap it with %w at
// the response-check site so a consumer can recover the code with
// errors.As(err, &se) regardless of any sentinel (e.g. ErrUnreachable) the
// client also composes in for its breaker/degrade logic. Its presence means the
// backend answered — i.e. it is reachable — as opposed to a transport failure,
// which carries no StatusError.
type StatusError struct {
	Code int    // HTTP status code the backend returned
	Body string // trimmed, length-capped response body (may be empty)
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("http %d", e.Code)
	}
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

// Retryable reports whether the status is a transient outage worth retrying
// (429 rate-limit or any 5xx) rather than a terminal client-side error (4xx).
func (e *StatusError) Retryable() bool {
	return e.Code == 429 || e.Code >= 500
}
