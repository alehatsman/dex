// Package health diagnoses whether dex's configured backends (embed, chat,
// rerank) are able to serve, and — when they can't — what to tell the operator.
//
// It owns the check/status vocabulary and the classification logic shared by
// `dex doctor`, `dex doctor --deep`, `dex setup`, and `dex index status`. The
// caller supplies the Probe list — env wiring and client construction live at
// the command layer — and health turns probe outcomes into classified,
// hint-carrying Checks. Rendering stays with the caller.
package health

import (
	"context"
	"fmt"

	"github.com/alehatsman/dex/internal/embed"
)

// Status classifies the outcome of one check.
type Status int

const (
	OK   Status = iota // ✓
	Warn               // ⚠ non-critical problem
	Fail               // ✗ critical failure
	Skip               // — not configured / not applicable
)

// Check is one labelled diagnostic row: a name, an outcome, a human-readable
// detail, optional fix hints, and whether a Fail here is fatal to the caller.
type Check struct {
	Name     string
	Status   Status
	Detail   string
	Hints    []string // rendered as "→ <hint>" under the row
	Critical bool     // Fail on a critical check → the caller exits non-zero
}

// Probe captures one configured backend to diagnose. Health is nil for a
// backend that isn't wired (unset opt-in URL) — those skip the network call and
// report the pre-set Status string. Deep sends one minimal *real* capability
// request (embed a string, a tiny completion, rerank a pair); nil when the
// backend isn't configured. Only deep mode calls it — see CheckEndpointsDeep.
type Probe struct {
	Name   string
	URL    string
	Model  string
	Health func(context.Context) error
	Deep   func(context.Context) error
	Status string // ok | UNREACHABLE | not configured
}

// CheckEndpoints fans out each probe's liveness Health check concurrently and
// returns one classified Check per probe. Liveness only — a metadata GET that
// never loads a model, so a cold backend can't be misreported as unreachable
// (#78). Probes without a Health func are reported as skipped.
func CheckEndpoints(ctx context.Context, probes []Probe) []Check {
	type result struct {
		name string
		err  error
	}
	ch := make(chan result, len(probes))
	launched := 0
	for i := range probes {
		if probes[i].Health == nil {
			continue
		}
		p := &probes[i]
		launched++
		go func() { ch <- result{p.Name, p.Health(ctx)} }()
	}

	errs := make(map[string]error, launched)
	for range launched {
		r := <-ch
		errs[r.name] = r.err
	}

	out := make([]Check, 0, len(probes))
	for _, p := range probes {
		out = append(out, classifyEndpoint(p, errs[p.Name]))
	}
	return out
}

// classifyEndpoint maps one liveness outcome into a Check. embed is the only
// backend whose unreachability is critical (indexing can't proceed without it);
// chat/rerank degrade, so their failures are warnings.
func classifyEndpoint(p Probe, err error) Check {
	if p.Health == nil {
		return Check{Name: p.Name, Status: Skip, Detail: p.Status}
	}

	if err != nil {
		status := Warn
		critical := false
		if p.Name == "embed" {
			status = Fail
			critical = true
		}
		return Check{
			Name:     p.Name,
			Status:   status,
			Critical: critical,
			Detail:   fmt.Sprintf("UNREACHABLE  (%s)", p.URL),
			Hints:    endpointHints(p.Name, p.URL),
		}
	}

	detail := p.URL
	if p.Model != "" {
		detail = p.URL + "  " + p.Model
	}
	return Check{Name: p.Name, Status: OK, Detail: detail}
}

// endpointHints offers a targeted fix for an unreachable backend, best-effort
// detecting a running-but-model-less ollama for the embed case.
func endpointHints(name, url string) []string {
	switch name {
	case "embed":
		if scan, ok := embed.ScanOllama(context.Background()); ok {
			if len(scan.EmbedModels) == 0 {
				return []string{
					fmt.Sprintf("ollama is running but has no embed model — run: ollama pull %s", embed.DefaultPullModel),
					"or: dex reindex --pull-model <path>",
				}
			}
			return []string{"check that the embedding service is running at " + url}
		}
		return []string{
			"start ollama: ollama serve",
			fmt.Sprintf("then: ollama pull %s", embed.DefaultPullModel),
			"or set DEX_EMBED_URL to a running OpenAI-compatible embedding service",
		}
	case "chat":
		return []string{
			"start ollama (auto-detected) or set DEX_CHAT_URL",
			"chat is required for: dex ask, dex generate, dex read --mode=summary",
		}
	}
	return nil
}
