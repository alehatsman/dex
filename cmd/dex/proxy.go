package main

// `dex proxy` — loopback Anthropic API pass-through (epic #232, spike #235).
//
// Sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL and
// forwards /v1/messages verbatim, streaming SSE responses through unbuffered.
// This cut does NO compression — it proves the seam works end-to-end and logs
// a per-request input-token baseline for the follow-up compression tickets.
//
//	export ANTHROPIC_BASE_URL=http://127.0.0.1:8788
//	dex proxy
//
// Note: a non-first-party base URL disables MCP tool search unless
// ENABLE_TOOL_SEARCH=true is also exported — required if the proxy forwards
// tool_reference blocks.

import (
	"context"
	"flag"
	"fmt"

	"github.com/alehatsman/dex/internal/proxy"
)

func cmdProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	setHelp(fs,
		"Loopback Anthropic API pass-through; logs a per-request input-token baseline (no compression yet).",
		"dex proxy [--addr 127.0.0.1:8788] [--upstream https://api.anthropic.com]",
		`dex proxy`,
		`dex proxy --addr 127.0.0.1:9000`,
		`ANTHROPIC_BASE_URL=http://127.0.0.1:8788 ENABLE_TOOL_SEARCH=true claude`,
	)
	addr := fs.String("addr", "127.0.0.1:8788",
		"Loopback listen address. Non-loopback binds are rejected (the proxy handles API keys).")
	upstream := fs.String("upstream", proxy.DefaultUpstream,
		"Upstream API base URL requests are forwarded to.")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("proxy takes no positional args (got %v)", fs.Args())
	}

	fmt.Printf("dex proxy\n")
	fmt.Printf("  addr=%s  upstream=%s\n", *addr, *upstream)
	fmt.Printf("  wire it up: export ANTHROPIC_BASE_URL=http://%s\n", *addr)
	fmt.Printf("  (also export ENABLE_TOOL_SEARCH=true when forwarding tool_reference blocks)\n")

	return proxy.Run(ctx, proxy.Options{
		Addr:     *addr,
		Upstream: *upstream,
		Logger:   cliLogger(),
	})
}
