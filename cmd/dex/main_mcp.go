package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/mcp"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/retrieve"
)

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	remote := fs.String("remote", "", "run as a stdio->REST shim against a remote `dex serve` at this base URL (e.g. http://host:8080); the local index is not used. Bearer token from DEX_SERVE_TOKEN")
	projectID := fs.String("project-id", "", "remote dex project id (sha256 of the canonical host root) to bind tool calls to; with --remote, required unless --project-root is given or the remote serves exactly one project")
	projectRoot := fs.String("project-root", "", "with --remote, compute the project id locally from this path (same-host convenience; the wrong id from a container whose checkout path differs from the host root — use --project-id there)")
	maintenance := fs.Bool("maintenance", false, "run a stub server that registers all tools but returns an immediate maintenance error on every call; agents fall back to native tools instead of hanging on timeouts")
	reason := fs.String("reason", "", "with --maintenance, a short message explaining why dex is unavailable (e.g. 'upgrading GPU drivers')")
	setHelp(fs,
		"Run dex as an MCP server over stdio (canonical entrypoint for Claude Code).\n"+
			"With --remote, run as a thin shim: speak MCP on stdio and proxy every tool\n"+
			"call to a remote `dex serve` REST endpoint (bearer from DEX_SERVE_TOKEN).\n"+
			"With --maintenance, run a stub that immediately signals agents to use native\n"+
			"tools (Read, Bash/grep) — use this during upgrades or outages.",
		"dex mcp [--remote <url> [--project-id <sha> | --project-root <path>]] [--maintenance [--reason <msg>]]")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("mcp takes no arguments (got %v)", fs.Args())
	}

	if *maintenance {
		if *remote != "" || *projectID != "" || *projectRoot != "" {
			return fmt.Errorf("--maintenance is incompatible with --remote/--project-id/--project-root")
		}
		return mcp.RunStdioMaintenance(ctx, *reason)
	}
	if *reason != "" {
		return fmt.Errorf("--reason is only valid with --maintenance")
	}
	if *remote != "" {
		return runRemoteMCP(ctx, *remote, *projectID, *projectRoot)
	}
	if *projectID != "" || *projectRoot != "" {
		return fmt.Errorf("--project-id/--project-root are only valid with --remote")
	}

	base, err := indexDir()
	if err != nil {
		return err
	}
	srv, _ := newServerFromEnv(base)
	return srv.RunStdio(ctx)
}

// runRemoteMCP launches the `dex mcp --remote` shim: an MCP stdio server
// whose tool calls are proxied to a remote `dex serve` daemon, bound to a
// single project id. The id is resolved in priority order: explicit
// --project-id, then --project-root computed locally (only correct when the
// shim runs on the same host with the same paths), then auto-discovery via
// the daemon's registry when it serves exactly one project.
func runRemoteMCP(ctx context.Context, baseURL, projectID, projectRoot string) error {
	token := os.Getenv("DEX_SERVE_TOKEN")

	id := projectID
	switch {
	case id != "":
		// explicit id — trust it (the only correct option from a container,
		// whose /work realpath differs from the canonical host root).
	case projectRoot != "":
		computed, err := mcp.ProjectID(projectRoot)
		if err != nil {
			return fmt.Errorf("--project-root %q: %w", projectRoot, err)
		}
		id = computed
	default:
		projects, err := mcp.ListRemoteProjects(ctx, baseURL, token, nil)
		if err != nil {
			return fmt.Errorf("resolve project id from %s: %w (pass --project-id explicitly)", baseURL, err)
		}
		switch len(projects) {
		case 0:
			return fmt.Errorf("remote %s serves no projects", baseURL)
		case 1:
			id = projects[0].ID
		default:
			var b strings.Builder
			for _, p := range projects {
				fmt.Fprintf(&b, "\n  %s  %s", p.ID, p.Root)
			}
			return fmt.Errorf("remote %s serves %d projects; pass --project-id <sha>:%s", baseURL, len(projects), b.String())
		}
	}

	return mcp.RunStdioRemote(ctx, mcp.RemoteOptions{
		BaseURL:   baseURL,
		Token:     token,
		ProjectID: id,
	})
}

// newServerFromEnv builds a fully-wired *mcp.Server from the current
// environment. Used by both `cmdMCP` (stdio server) and `cmdContext`
// (one-shot CLI invocation of the context router). The HTTP clients
// are lazy — they don't dial until invoked — so wiring all of them
// is cheap even when the context router only uses Embed.
//
// Returns the shared rerank client as the second value so callers
// that need it for separate purposes (e.g. health reporting) don't
// have to redundantly construct another instance.
func newServerFromEnv(base string) (*mcp.Server, rerank.HealthChecker) {
	var rerankClient rerank.HealthChecker = newRerankClient()
	opts := storeOpts()
	var rerankSvc retrieve.Service
	if rerankClient != nil {
		// Wrap in a circuit breaker so a hung rerank backend doesn't
		// drag every search through its full timeout for the next 30s
		// after a string of failures. The same wrapper is shared by
		// status (RerankClient) and the rerank service (StoreOpts.Rerank
		// hook + Server.Retrieve) so the breaker state in `dex index
		// status` reflects what callers actually see.
		//
		// One service (and one score cache) is built here and shared: the
		// store rerank hook and the mcp search tools both route through it,
		// so an identical (query, pool) is reranked once per process — the
		// long-lived equivalent of the CLI's per-command cache (#473).
		rerankClient = rerank.NewBreaker(rerankClient, 3, 30*time.Second)
		rerankSvc = newRerankService(rerankClient, opts.DefinitionBoost)
		opts.Rerank = rerankSvc.RerankFused
		opts.MaxCandidatePool = rerankPool()
	}
	chatClient := newChatClient()
	expandClient := newExpandClient(chatClient)
	srv := &mcp.Server{
		EmbedClient:  newEmbedClient(""),
		ChatClient:   chatClient,
		RerankClient: rerankClient,
		ExpandMode:   expandDefaultMode(expandClient),
		IndexDir:     base,
		StoreOpts:    opts,
		Retrieve:     rerankSvc,
		AutoWatch:    autoWatchConfigFromEnv(),
	}
	// Only populate the interface field when a client is actually configured.
	// newExpandClient returns a *chat.Client, so assigning its nil value
	// directly would leave ExpandClient a non-nil interface wrapping a nil
	// pointer — the per-request expand=on|full override would then deref it
	// and panic instead of degrading to the documented no-op (#502).
	if expandClient != nil {
		srv.ExpandClient = expandClient
	}
	return srv, rerankClient
}

// autoWatchConfigFromEnv reads DEX_MCP_AUTOWATCH to build a config for the
// MCP server's lazy per-project watchers. Default: enabled. Each watcher
// refreshes the chunk and graph lanes on change (see runWatcher's
// AfterIndex hook); the debounce window is DEX_WATCH_DEBOUNCE and the
// burst max-delay cap is DEX_WATCH_MAX_DELAY.
func autoWatchConfigFromEnv() mcp.AutoWatchConfig {
	enabled := envBool("DEX_MCP_AUTOWATCH", true)
	if !enabled {
		return mcp.AutoWatchConfig{} // zero value disables
	}
	return mcp.AutoWatchConfig{
		Enabled:  true,
		Debounce: envDuration("DEX_WATCH_DEBOUNCE", 500*time.Millisecond),
		MaxDelay: envDuration("DEX_WATCH_MAX_DELAY", 5*time.Second),
		Logger:   cliLogger(),
	}
}
