// Output rendering for the CLI subcommands. Splitting these out keeps
// main.go focused on dispatch + env wiring, and makes it obvious which
// pieces are "presentation only" vs "real work".
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/health"
)

// collectEndpoints builds the probe list the status and doctor commands
// consume. This is the injection seam for internal/health: it does the env
// wiring (embed/chat always present via defaults; rerank opt-in) and hands
// health.CheckEndpoints / CheckEndpointsDeep a ready list of probes with their
// liveness + deep-capability closures bound.
func collectEndpoints() []health.Probe {
	probes := []health.Probe{}

	// A nil embedder is the lean profile (DEX_EMBED_ENGINE=none / bm25-only):
	// no embedding backend is wired, so probe nothing rather than dereferencing
	// it (#545). Mirrors the opt-in rerank branch below.
	if em := newEmbedClient(""); em != nil {
		probes = append(probes, health.Probe{Name: "embed", URL: em.Endpoint(), Model: em.ModelName(), Health: em.Health,
			Deep: func(ctx context.Context) error {
				vecs, err := em.Embed(ctx, []string{health.DeepProbeText})
				if err != nil {
					return err
				}
				if len(vecs) != 1 || len(vecs[0]) == 0 {
					return fmt.Errorf("returned %d vectors (want 1 non-empty)", len(vecs))
				}
				return nil
			}})
	} else {
		probes = append(probes, health.Probe{Name: "embed", Status: "not configured"})
	}

	// Chat is opt-in like rerank: probe it only when a chat model was actually
	// wired. A bare DEX_CHAT_URL pointing at an embed-only ollama would otherwise
	// probe a fabricated default model and report DEGRADED for a capability the
	// user never configured (#133). Mirrors the embed/rerank "not configured" arm.
	if cc, ok := newChatClientConfigured(); ok {
		probes = append(probes, health.Probe{Name: "chat", URL: cc.BaseURL, Model: cc.Model, Health: cc.Health,
			Deep: func(ctx context.Context) error {
				_, err := cc.Generate(ctx, []chat.Message{{Role: "user", Content: health.DeepProbeText}}, chat.Options{MaxTokens: 1})
				return err
			}})
	} else {
		probes = append(probes, health.Probe{Name: "chat", Status: "not configured"})
	}

	if rc := newRerankClient(); rc != nil {
		probes = append(probes, health.Probe{Name: "rerank", URL: rc.Endpoint(), Model: rc.ModelName(), Health: rc.Health,
			Deep: func(ctx context.Context) error {
				scores, err := rc.Rerank(ctx, health.DeepProbeText, []string{"a candidate document"})
				if err != nil {
					return err
				}
				if len(scores) == 0 {
					return fmt.Errorf("returned no scores for a 1-document rerank")
				}
				return nil
			}})
	} else {
		probes = append(probes, health.Probe{Name: "rerank", Status: "not configured"})
	}

	return probes
}

// printEndpoints fans out concurrent health checks for every probe with
// a configured URL, then renders an aligned table under a section
// header.
//
// Column order is (NAME, STATUS, MODEL, URL) — status sits next to
// the name so a quick glance scans down a single column to spot any
// failures, instead of having to skip past two wide columns first.
func printEndpoints(ctx context.Context) {
	probes := collectEndpoints()

	var wg sync.WaitGroup
	for i := range probes {
		if probes[i].Health == nil {
			continue
		}
		wg.Add(1)
		go func(p *health.Probe) {
			defer wg.Done()
			err := p.Health(ctx)
			switch {
			case err == nil:
				p.Status = "ok"
			case errors.Is(err, chat.ErrModelNotFound):
				p.Status = fmt.Sprintf("DEGRADED model not found: %s", p.Model)
			default:
				p.Status = "UNREACHABLE"
			}
		}(&probes[i])
	}
	wg.Wait()

	// Count reachable for the section heading so users can spot a
	// degraded backend without reading the full table.
	reachable := 0
	for _, p := range probes {
		if p.Status == "ok" {
			reachable++
		}
	}

	// Column widths derived from the data PLUS the literal header
	// labels so the heading row aligns under the data even when the
	// widest data cell is narrower than the label.
	headers := struct{ name, status, model, url string }{"NAME", "STATUS", "MODEL", "URL"}
	nameW := len(headers.name)
	statusW := len(headers.status)
	modelW := len(headers.model)
	urlW := len(headers.url)
	for _, p := range probes {
		nameW = max(nameW, len(p.Name))
		statusW = max(statusW, len(p.Status))
		modelW = max(modelW, len(displayCell(p.Model)))
		urlW = max(urlW, len(displayCell(p.URL)))
	}

	fmt.Printf("endpoints (%d reachable)\n", reachable)
	fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
		nameW, headers.name,
		statusW, headers.status,
		modelW, headers.model,
		headers.url)
	for _, p := range probes {
		fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
			nameW, p.Name,
			statusW, p.Status,
			modelW, displayCell(p.Model),
			displayCell(p.URL))
	}

	// When the embed endpoint is unreachable and ollama is running but has no
	// embed models, offer a one-liner to fix it.
	var embedUnreachable bool
	for _, p := range probes {
		if p.Name == "embed" && p.Status == "UNREACHABLE" {
			embedUnreachable = true
			break
		}
	}
	if embedUnreachable {
		if scan, ok := embed.ScanOllama(context.Background()); ok && len(scan.EmbedModels) == 0 {
			fmt.Printf("\nhint: ollama is running but has no embedding model — run:\n")
			fmt.Printf("  ollama pull %s\n", embed.DefaultPullModel)
			fmt.Printf("or: dex reindex --pull-model <path>\n")
		}
	}
}

func displayCell(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
