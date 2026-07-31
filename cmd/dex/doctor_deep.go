// `dex doctor --deep` — opt-in backend readiness checks.
//
// The default doctor (and `status`) probe liveness only: a metadata GET that
// never touches the model, so a cold model load can't falsely report a healthy
// backend as UNREACHABLE (#78). Deep mode trades that guarantee for a stronger
// signal: it sends one minimal *real* capability request per configured backend
// (embed a string, a 1-token completion, rerank a pair) and classifies the
// outcome — usable / model-not-ready / unreachable / cold-timeout — so an
// operator can tell "listening" from "actually able to serve" before indexing.
package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
)

// deepProbeText is the throwaway input sent to every backend in deep mode.
const deepProbeText = "dex readiness probe"

// deepTimeouts bounds each backend's capability call. Chat gets the longest
// budget because a cold LLM load dwarfs an embed/rerank warm-up.
var deepTimeouts = map[string]time.Duration{
	"embed":  15 * time.Second,
	"rerank": 15 * time.Second,
	"chat":   45 * time.Second,
}

// checkEndpointsDeep runs each configured backend's deep capability probe
// concurrently under a per-backend timeout and returns one classified row per
// probe. Probes without a deep closure (unconfigured backends) are skipped.
func checkEndpointsDeep(ctx context.Context) []doctorCheck {
	probes := collectEndpoints()

	type result struct {
		name string
		err  error
	}
	ch := make(chan result)
	launched := 0
	for i := range probes {
		if probes[i].deep == nil {
			continue
		}
		p := &probes[i]
		launched++
		to := deepTimeouts[p.name]
		if to == 0 {
			to = 15 * time.Second
		}
		go func() {
			cctx, cancel := context.WithTimeout(ctx, to)
			defer cancel()
			ch <- result{p.name, p.deep(cctx)}
		}()
	}

	errsByName := make(map[string]error, launched)
	for range launched {
		r := <-ch
		errsByName[r.name] = r.err
	}

	out := make([]doctorCheck, 0, launched)
	for _, p := range probes {
		if p.deep == nil {
			continue
		}
		out = append(out, classifyDeep(p, errsByName[p.name]))
	}
	return out
}

// classifyDeep maps a deep probe's error into a labelled check. embed is the
// only backend whose readiness is critical (indexing can't proceed without it);
// chat/rerank degrade, so their deep failures are warnings. A cold-load timeout
// is always a warning — it's retryable, not a misconfiguration.
func classifyDeep(p endpointProbe, err error) doctorCheck {
	name := p.name + " (deep)"
	critical := p.name == "embed"
	failStatus := docWarn
	if critical {
		failStatus = docFail
	}

	if err == nil {
		detail := "usable"
		if p.model != "" {
			detail = "usable  " + p.model
		}
		return doctorCheck{name: name, status: docOK, detail: detail}
	}

	// Cold model still loading — reachable, retryable, never a misconfiguration.
	if errors.Is(err, context.DeadlineExceeded) {
		return doctorCheck{
			name: name, status: docWarn,
			detail: fmt.Sprintf("readiness timed out after %s (model may be cold-loading)", deepTimeouts[p.name]),
			hints:  []string{"pre-warm the model, then re-run: dex doctor --deep"},
		}
	}

	// If the backend answered with an HTTP status, it is reachable — classify by
	// the code rather than trusting the transport sentinel. The embed/rerank
	// clients wrap 5xx/429 as ErrUnreachable so the breaker degrades search
	// (#445), but for a *readiness read* those mean "up but can't serve right
	// now", which is a retryable warning, not an outage.
	if code, ok := httpStatusFromErr(err); ok {
		if code == 429 || code >= 500 {
			return doctorCheck{
				name: name, status: docWarn, // transient, retryable — not critical
				detail: fmt.Sprintf("reachable but overloaded/restarting (http %d) — retry", code),
				hints:  []string{"backend is up but can't serve right now; re-run once load subsides"},
			}
		}
		// 4xx: model not served / bad request / incompatible response.
		return doctorCheck{
			name: name, status: failStatus, critical: critical,
			detail: "reachable but not ready: " + err.Error(),
			hints:  deepNotReadyHints(p.name, p.model, err),
		}
	}

	// No HTTP status in the error → a transport-level failure (dial/timeout
	// wrapped as the client's unreachable sentinel, or a bare network error).
	if errors.Is(err, embed.ErrUnreachable) || errors.Is(err, chat.ErrUnreachable) || errors.Is(err, rerank.ErrUnreachable) {
		return doctorCheck{
			name: name, status: failStatus, critical: critical,
			detail: "UNREACHABLE  (" + p.url + ")",
			hints:  endpointHints(p.name, p.url),
		}
	}

	// Reachable but the response was unusable (empty vectors, decode error, …).
	return doctorCheck{
		name: name, status: failStatus, critical: critical,
		detail: "reachable but not ready: " + err.Error(),
		hints:  deepNotReadyHints(p.name, p.model, err),
	}
}

// httpStatusFromErr extracts an HTTP status code from a client error whose
// message carries the "http <code>:" marker the embed/chat/rerank clients use
// for non-2xx responses. Its presence means the backend answered (is reachable);
// absence means the failure was transport-level.
func httpStatusFromErr(err error) (int, bool) {
	m := httpStatusRe.FindStringSubmatch(strings.ToLower(err.Error()))
	if m == nil {
		return 0, false
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return code, true
}

var httpStatusRe = regexp.MustCompile(`http (\d{3})`)

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
