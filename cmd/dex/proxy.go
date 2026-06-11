package main

// `dex proxy` — loopback Anthropic API pass-through (epic #232).
//
// Sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL and
// runs each /v1/messages request through history pruning and tool_result
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

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/alehatsman/dex/internal/proxy"
)

func cmdProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	setHelp(fs,
		"Loopback Anthropic API pass-through with history pruning and tool_result compression.",
		"dex proxy [--addr 127.0.0.1:8788] [--upstream https://api.anthropic.com] [--stats]",
		`dex proxy`,
		`dex proxy --addr 127.0.0.1:9000`,
		`dex proxy --stats`,
		`ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ENABLE_TOOL_SEARCH=true claude`,
	)
	addr := fs.String("addr", "127.0.0.1:8788",
		"Loopback listen address. Non-loopback binds are rejected.")
	upstream := fs.String("upstream", proxy.DefaultUpstream,
		"Upstream API base URL requests are forwarded to.")
	statsFlag := fs.Bool("stats", false,
		"Fetch and print a token-savings snapshot from a running proxy, then exit.")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("proxy takes no positional args (got %v)", fs.Args())
	}

	if *statsFlag {
		return printProxyStats(ctx, *addr)
	}

	fmt.Printf("dex proxy\n")
	fmt.Printf("  addr=%s  upstream=%s\n", *addr, *upstream)
	fmt.Printf("  wire it up: export ANTHROPIC_BASE_URL=http://%s\n", *addr)
	fmt.Printf("  (also export ENABLE_TOOL_SEARCH=true when forwarding tool_reference blocks)\n")
	fmt.Printf("  stats: dex proxy --stats\n")

	return proxy.Run(ctx, proxy.Options{
		Addr:     *addr,
		Upstream: *upstream,
		Logger:   cliLogger(),
	})
}

// printProxyStats fetches the /stats snapshot from a running proxy and prints it.
func printProxyStats(ctx context.Context, addr string) error {
	snap, err := proxy.FetchStats(ctx, addr)
	if err != nil {
		return fmt.Errorf("could not reach proxy at %s: %w\n  (start with: dex proxy --addr %s)", addr, err, addr)
	}

	pct := snap.CompressionRatio * 100
	fmt.Fprintf(os.Stdout, "dex proxy stats  addr=%s\n", addr)
	fmt.Fprintf(os.Stdout, "  requests : %d total, %d compressed\n", snap.RequestsTotal, snap.RequestsCompressed)
	fmt.Fprintf(os.Stdout, "  tokens   : %d before → %d after  (%d saved, %.1f%%)\n",
		snap.TokensBefore, snap.TokensAfter, snap.TokensSaved, pct)
	fmt.Fprintln(os.Stdout)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}
