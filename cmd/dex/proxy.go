package main

// `dex proxy` — loopback Anthropic API pass-through (epic #232).
//
// Sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL and
// runs each /v1/messages request through history pruning and tool-description
// compression before forwarding. SSE streaming passes through unbuffered.
//
//	export ANTHROPIC_BASE_URL=http://127.0.0.1:8788
//	dex proxy
//
// Token counters are tracked per-session and exposed via GET /stats.
// Use --stats to fetch and print a snapshot from a running proxy:
//
//	dex proxy --stats
//	dex proxy --stats --addr 127.0.0.1:9000
//
// Note: a non-first-party base URL disables MCP tool search unless
// ENABLE_TOOL_SEARCH=true is also exported.
//
// Security posture (#240): the proxy handles the agent's Anthropic API key, so
// it binds loopback-only by default and refuses a non-loopback --addr unless
// DEX_PROXY_TOKEN is set. When set, incoming requests must carry the token in
// the X-Dex-Proxy-Token header; the upstream credential is forwarded untouched
// and never persisted, and request/response bodies are never logged.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/proxy"
)

func cmdProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	setHelp(fs,
		"Loopback Anthropic API pass-through with history pruning and tool-description compression.",
		"dex proxy [--addr 127.0.0.1:8788] [--upstream https://api.anthropic.com] [--stats]",
		`dex proxy`,
		`dex proxy --addr 127.0.0.1:9000`,
		`dex proxy --stats`,
		`ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ENABLE_TOOL_SEARCH=true claude`,
	)
	addr := fs.String("addr", "127.0.0.1:8788",
		"Listen address. Loopback-only unless DEX_PROXY_TOKEN is set.")
	upstream := fs.String("upstream", proxy.DefaultUpstream,
		"Upstream API base URL requests are forwarded to.")
	statsFlag := fs.Bool("stats", false,
		"Fetch and print a token-savings snapshot from a running proxy, then exit.")
	toolDescFlag := fs.String("tool-desc", "",
		"MCP tool-description compression: full|terse|lazy (default full; env DEX_PROXY_TOOL_DESC). Forced full when ENABLE_TOOL_SEARCH is set.")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("proxy takes no positional args (got %v)", fs.Args())
	}

	// DEX_PROXY_TOKEN gates incoming requests and unlocks a non-loopback bind.
	// It is a secret: read only from the environment, never from config.yml.
	token := strings.TrimSpace(os.Getenv("DEX_PROXY_TOKEN"))

	if *statsFlag {
		return printProxyStats(ctx, *addr, token)
	}

	// Tool-description compression mode (#242): --tool-desc, falling back to
	// DEX_PROXY_TOOL_DESC, default full (no-op). Not a secret, so flag + env
	// are both fine. Honor the caveat — when ENABLE_TOOL_SEARCH /
	// tool_reference forwarding is in play the agent relies on full tool docs
	// to pick tools, so clamp any aggressive mode back to full.
	toolDescRaw := *toolDescFlag
	if strings.TrimSpace(toolDescRaw) == "" {
		toolDescRaw = os.Getenv("DEX_PROXY_TOOL_DESC")
	}
	toolDescMode := proxy.ParseToolDescMode(toolDescRaw)
	if toolDescMode != proxy.ToolDescFull && envBool("ENABLE_TOOL_SEARCH", false) {
		fmt.Printf("  tool-desc: %s requested but ENABLE_TOOL_SEARCH is set → forced full (preserves tool-selection docs)\n", toolDescMode)
		toolDescMode = proxy.ToolDescFull
	}

	fmt.Printf("dex proxy\n")
	fmt.Printf("  addr=%s  upstream=%s  auth=%v\n", *addr, *upstream, token != "")
	fmt.Printf("  wire it up: export ANTHROPIC_BASE_URL=http://%s\n", *addr)
	fmt.Printf("  (also export ENABLE_TOOL_SEARCH=true when forwarding tool_reference blocks)\n")
	if token != "" {
		fmt.Printf("  auth: clients must send header %s: <DEX_PROXY_TOKEN>\n", proxy.ProxyTokenHeader)
	}
	fmt.Printf("  tool-desc: %s\n", toolDescMode)
	fmt.Printf("  stats: dex proxy --stats\n")

	return proxy.Run(ctx, proxy.Options{
		Addr:         *addr,
		Upstream:     *upstream,
		Logger:       cliLogger(),
		Token:        token,
		ToolDescMode: toolDescMode,
	})
}

// printProxyStats fetches the /stats snapshot from a running proxy and prints it.
func printProxyStats(ctx context.Context, addr, token string) error {
	snap, err := proxy.FetchStats(ctx, addr, token)
	if err != nil {
		return fmt.Errorf("could not reach proxy at %s: %w\n  (start with: dex proxy --addr %s)", addr, err, addr)
	}

	pct := snap.CompressionRatio * 100
	fmt.Fprintf(os.Stdout, "dex proxy stats  addr=%s\n", addr)
	fmt.Fprintf(os.Stdout, "  requests : %d total, %d compressed\n", snap.RequestsTotal, snap.RequestsCompressed)
	fmt.Fprintf(os.Stdout, "  tokens   : %d before → %d after  (%d saved, %.1f%%)\n",
		snap.TokensBefore, snap.TokensAfter, snap.TokensSaved, pct)
	fmt.Fprintf(os.Stdout, "  re-reads : %d files re-read after prune  (%d tokens re-fetched)\n",
		snap.ReReadsAfterStub, snap.ReReadTokens)
	fmt.Fprintln(os.Stdout)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}
