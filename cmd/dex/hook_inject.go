package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/proj"
)

// hookInject handles UserPromptSubmit. It runs a dex ask query on the prompt
// and emits {"additionalContext": "..."} so Claude sees relevant file paths
// before processing the turn. Also prepends a one-time-per-session nudge when
// routing rules are stale or drifted, and a per-prompt nudge when dex MCP
// schemas have not yet been loaded via ToolSearch. Silent on any error.
func hookInject(ctx context.Context) error {
	raw := hookReadStdin()
	if len(raw) == 0 {
		return nil
	}

	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	nudge := rulesNudge()
	if sn := schemasNudge(); sn != "" {
		if nudge != "" {
			nudge += "\n"
		}
		nudge += sn
	}

	// Skip very short prompts (confirmations, "yes", "ok", etc.) — not
	// worth a round-trip to the index for sub-4-word inputs.
	if len(strings.Fields(payload.Prompt)) < 4 {
		return emitInjectContext(nudge, "")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return emitInjectContext(nudge, "")
	}
	base, err := indexDir()
	if err != nil {
		return emitInjectContext(nudge, "")
	}
	p, err := proj.Resolve(cwd, base)
	if err != nil {
		return emitInjectContext(nudge, "")
	}

	// 10 s budget — the hook runs synchronously before Claude processes the
	// turn, so latency is visible to the user.
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	s, _ := newServerFromEnv(base)
	_, out, err := s.ContextRouter(tctx, mcp.ContextInput{
		ProjectRoot: p.Root,
		Question:    payload.Prompt,
		K:           6,
		// NoInline=true: inject only paths + reasons, not raw content.
		// The content would bloat every turn; Claude can Read the files.
		NoInline: true,
	})
	if err != nil || out.Status != "ok" {
		return emitInjectContext(nudge, "")
	}

	return emitInjectContext(nudge, buildInjectContext(out))
}

// emitInjectContext encodes additionalContext combining nudge and ac.
// Emits nothing and returns nil when both are empty.
func emitInjectContext(nudge, ac string) error {
	combined := nudge
	if ac != "" {
		if combined != "" {
			combined += "\n\n"
		}
		combined += ac
	}
	if combined == "" {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"additionalContext": combined})
}

// rulesNudge returns a one-time-per-session warning when routing rules are
// stale or drifted. Returns "" when rules are in sync or the nudge already
// fired recently (debounced by a sentinel file with an 8 h TTL). Fails open.
func rulesNudge() string {
	st, _ := checkRulesStatus()
	if st == rulesInSync {
		return ""
	}

	sentinel := rulesNudgeSentinelPath()
	if sentinel != "" {
		if fi, err := os.Stat(sentinel); err == nil {
			if time.Since(fi.ModTime()) < 8*time.Hour {
				return "" // already nudged this session
			}
		}
		// Touch sentinel before emitting so concurrent hook invocations don't double-fire.
		if mkErr := os.MkdirAll(filepath.Dir(sentinel), 0o755); mkErr == nil {
			if f, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
				_ = f.Close()
			}
		}
	}

	switch st {
	case rulesMissing, rulesNoMarkers:
		return "[DEX] routing rules not installed — run `dex setup`"
	case rulesStale:
		return "[DEX] routing rules are outdated — run `dex setup`"
	case rulesDrifted:
		return "[DEX] routing rules have drifted from canonical — run `dex setup` to restore"
	}
	return ""
}

func rulesNudgeSentinelPath() string {
	dir := hookLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "rules-nudge-sentinel")
}

// schemasNudge returns a reminder to load dex MCP tool schemas via ToolSearch
// if they have not been loaded yet this session. It fires on every prompt until
// hookObserve sees a ToolSearch call and creates the schemas-loaded sentinel.
// The sentinel expires after 30 minutes so a new session always gets the nudge.
func schemasNudge() string {
	sentinel := schemasLoadedSentinelPath()
	if sentinel == "" {
		return ""
	}
	if fi, err := os.Stat(sentinel); err == nil && time.Since(fi.ModTime()) < 30*time.Minute {
		return "" // schemas loaded this session
	}
	return "[DEX] Schemas not loaded — call ToolSearch(query=\"select:mcp__dex__ask,mcp__dex__shell,mcp__dex__ls,mcp__dex__find,mcp__dex__grep,mcp__dex__read\") as your FIRST action before any other tool call."
}

func schemasLoadedSentinelPath() string {
	dir := hookLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "schemas-loaded-sentinel")
}

func buildInjectContext(out mcp.ContextOutput) string {
	if len(out.SuggestedReads) == 0 && len(out.Symbols) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Dex: relevant context\n\n")

	if len(out.SuggestedReads) > 0 {
		b.WriteString("Suggested reads:\n")
		for _, r := range out.SuggestedReads {
			if r.StartLine > 0 && r.EndLine > 0 {
				fmt.Fprintf(&b, "- %s:%d-%d", r.Path, r.StartLine, r.EndLine)
			} else {
				fmt.Fprintf(&b, "- %s", r.Path)
			}
			if r.Reason != "" {
				fmt.Fprintf(&b, " — %s", r.Reason)
			}
			b.WriteByte('\n')
		}
	}

	if len(out.Symbols) > 0 {
		b.WriteString("\nSymbols:\n")
		for _, sym := range out.Symbols {
			fmt.Fprintf(&b, "- %s %s (%s:%d)\n", sym.Kind, sym.QualifiedName, sym.Path, sym.StartLine)
		}
	}

	if out.NextAction != "" {
		b.WriteString("\nNext: ")
		b.WriteString(out.NextAction)
		b.WriteByte('\n')
	}
	return b.String()
}
