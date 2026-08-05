// Deep readiness — the opt-in `dex doctor --deep` capability probes.
//
// The default doctor (and `status`) probe liveness only: a metadata GET that
// never touches the model, so a cold model load can't falsely report a healthy
// backend as UNREACHABLE (#78). Deep mode trades that guarantee for a stronger
// signal: it sends one minimal *real* capability request per configured backend
// (embed a string, a 1-token completion, rerank a pair) and classifies the
// outcome — usable / model-not-ready / unreachable / cold-timeout — so an
// operator can tell "listening" from "actually able to serve" before indexing.

package health

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/backendhttp"
	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
)

// DeepProbeText is the throwaway input sent to every backend in deep mode.
// Callers building the Probe.Deep closures use it as the request payload.
const DeepProbeText = "dex readiness probe"

// deepTimeouts bounds each backend's capability call. Chat gets the longest
// budget because a cold LLM load dwarfs an embed/rerank warm-up.
var deepTimeouts = map[string]time.Duration{
	"embed":  15 * time.Second,
	"rerank": 15 * time.Second,
	"chat":   45 * time.Second,
}

// CheckEndpointsDeep runs each configured backend's deep capability probe
// concurrently under a per-backend timeout and returns one classified row per
// probe. Probes without a Deep closure (unconfigured backends) are skipped.
func CheckEndpointsDeep(ctx context.Context, probes []Probe) []Check {
	type result struct {
		name string
		err  error
	}
	ch := make(chan result)
	launched := 0
	for i := range probes {
		if probes[i].Deep == nil {
			continue
		}
		p := &probes[i]
		launched++
		to := deepTimeouts[p.Name]
		if to == 0 {
			to = 15 * time.Second
		}
		go func() {
			cctx, cancel := context.WithTimeout(ctx, to)
			defer cancel()
			ch <- result{p.Name, p.Deep(cctx)}
		}()
	}

	errsByName := make(map[string]error, launched)
	for range launched {
		r := <-ch
		errsByName[r.name] = r.err
	}

	out := make([]Check, 0, launched)
	for _, p := range probes {
		if p.Deep == nil {
			continue
		}
		out = append(out, ClassifyDeep(p, errsByName[p.Name]))
	}
	return out
}

// ClassifyDeep maps a deep probe's error into a labelled check. embed is the
// only backend whose readiness is critical (indexing can't proceed without it);
// chat/rerank degrade, so their deep failures are warnings. A cold-load timeout
// is always a warning — it's retryable, not a misconfiguration.
func ClassifyDeep(p Probe, err error) Check {
	name := p.Name + " (deep)"
	critical := p.Name == "embed"
	failStatus := Warn
	if critical {
		failStatus = Fail
	}

	if err == nil {
		detail := "usable"
		if p.Model != "" {
			detail = "usable  " + p.Model
		}
		return Check{Name: name, Status: OK, Detail: detail}
	}

	// Cold model still loading — reachable, retryable, never a misconfiguration.
	if errors.Is(err, context.DeadlineExceeded) {
		return Check{
			Name: name, Status: Warn,
			Detail: fmt.Sprintf("readiness timed out after %s (model may be cold-loading)", deepTimeouts[p.Name]),
			Hints:  []string{"pre-warm the model, then re-run: dex doctor --deep"},
		}
	}

	// If the backend answered with an HTTP status, it is reachable — classify by
	// the code rather than trusting the transport sentinel. The embed/rerank
	// clients compose a StatusError even when they also wrap 5xx/429 as
	// ErrUnreachable for the breaker (#445), so errors.As recovers the code; for
	// a *readiness read* a 5xx/429 means "up but can't serve right now", a
	// retryable warning rather than an outage.
	var se *backendhttp.StatusError
	if errors.As(err, &se) {
		if se.Retryable() {
			return Check{
				Name: name, Status: Warn, // transient, retryable — not critical
				Detail: fmt.Sprintf("reachable but overloaded/restarting (http %d) — retry", se.Code),
				Hints:  []string{"backend is up but can't serve right now; re-run once load subsides"},
			}
		}
		// 4xx: model not served / bad request / incompatible response.
		return Check{
			Name: name, Status: failStatus, Critical: critical,
			Detail: "reachable but not ready: " + err.Error(),
			Hints:  deepNotReadyHints(p.Name, p.Model, err),
		}
	}

	// No HTTP status in the error → a transport-level failure (dial/timeout
	// wrapped as the client's unreachable sentinel, or a bare network error).
	if errors.Is(err, embed.ErrUnreachable) || errors.Is(err, chat.ErrUnreachable) || errors.Is(err, rerank.ErrUnreachable) {
		return Check{
			Name: name, Status: failStatus, Critical: critical,
			Detail: "UNREACHABLE  (" + p.URL + ")",
			Hints:  endpointHints(p.Name, p.URL),
		}
	}

	// Reachable but the response was unusable (empty vectors, decode error, …).
	return Check{
		Name: name, Status: failStatus, Critical: critical,
		Detail: "reachable but not ready: " + err.Error(),
		Hints:  deepNotReadyHints(p.Name, p.Model, err),
	}
}

// deepNotReadyHints gives a targeted fix when a reachable backend rejects the
// probe. It best-effort-detects a "model not served" error to point at the
// right knob; otherwise it nudges toward the server logs.
func deepNotReadyHints(name, model string, err error) []string {
	s := strings.ToLower(err.Error())
	modelIssue := strings.Contains(s, "model") || strings.Contains(s, "not found") || strings.Contains(s, "404")
	if modelIssue {
		switch name {
		case "embed":
			return []string{
				fmt.Sprintf("model %q not served — pull/load it (ollama pull %s) or set DEX_EMBED_MODEL", model, model),
			}
		case "chat":
			return []string{
				fmt.Sprintf("model %q not served — load it on the backend or set DEX_CHAT_MODEL", model),
			}
		case "rerank":
			return []string{
				fmt.Sprintf("model %q not served — load it on the backend or set DEX_RERANK_MODEL", model),
			}
		}
	}
	return []string{"endpoint is reachable but rejected a minimal request — check the model config and server logs"}
}
