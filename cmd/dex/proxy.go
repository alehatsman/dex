package main

// `dex proxy` — loopback Anthropic API pass-through (epic #232).
//
// Sits between Claude Code and the Anthropic API at ANTHROPIC_BASE_URL and
// runs each /v1/messages request through model routing, history pruning, and
// tool-description compression before forwarding. SSE streaming passes through unbuffered.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/proxy"
	"github.com/alehatsman/dex/internal/slo"
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
	routeModelFlag := fs.String("route-model", "",
		"Token-count model routing: on|off (default off; env DEX_PROXY_ROUTE_MODEL).")
	routeLowThreshold := fs.Int("route-low-threshold", 0,
		"Input tokens below this → route-low-model (default 2000; env DEX_PROXY_ROUTE_LOW_THRESHOLD).")
	routeLowModel := fs.String("route-low-model", "",
		"Model for low-token turns (env DEX_PROXY_ROUTE_LOW_MODEL; default claude-haiku-4-5-20251001).")
	routeMidThreshold := fs.Int("route-mid-threshold", 0,
		"Input tokens below this → route-mid-model (default 20000; env DEX_PROXY_ROUTE_MID_THRESHOLD).")
	routeMidModel := fs.String("route-mid-model", "",
		"Model for mid-token turns (env DEX_PROXY_ROUTE_MID_MODEL; default claude-sonnet-4-6).")
	effortFlag := fs.String("effort", "",
		"Reasoning-effort budget: low|medium|high (env DEX_PROXY_EFFORT). Skipped if client already set effort or model is not a reasoning model.")
	ccrFlag := fs.Bool("ccr", false,
		"Content-addressed recovery: tee pruned reads to a content-addressed store and re-inject on demand (env DEX_PROXY_CCR). Off by default.")
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

	routeCfg := parseModelRouteConfig(*routeModelFlag, *routeLowThreshold, *routeLowModel, *routeMidThreshold, *routeMidModel)

	effortRaw := *effortFlag
	if strings.TrimSpace(effortRaw) == "" {
		effortRaw = os.Getenv("DEX_PROXY_EFFORT")
	}
	effort := proxy.ParseEffortLevel(effortRaw)

	// Content-addressed recovery (#597): --ccr flag, falling back to
	// DEX_PROXY_CCR. Not a secret, so flag + env are both fine.
	ccrEnabled := *ccrFlag || envBool("DEX_PROXY_CCR", false)

	// Budget event log (#60): one JSONL file per session under ~/.cache/dex/sessions/.
	sessionID := time.Now().UTC().Format("20060102T150405Z")
	bl, blErr := openBudgetLog(sessionID)
	if blErr != nil {
		fmt.Printf("  budget-log: disabled (%v)\n", blErr)
	}

	fmt.Printf("dex proxy\n")
	fmt.Printf("  addr=%s  upstream=%s  auth=%v\n", *addr, *upstream, token != "")
	fmt.Printf("  wire it up: export ANTHROPIC_BASE_URL=http://%s\n", *addr)
	fmt.Printf("  (also export ENABLE_TOOL_SEARCH=true when forwarding tool_reference blocks)\n")
	if token != "" {
		fmt.Printf("  auth: clients must send header %s: <DEX_PROXY_TOKEN>\n", proxy.ProxyTokenHeader)
	}
	fmt.Printf("  tool-desc: %s\n", toolDescMode)
	if routeCfg.Enabled {
		fmt.Printf("  route-model: on  low<%d→%s  mid<%d→%s\n",
			routeCfg.LowThreshold, routeCfg.LowModel,
			routeCfg.MidThreshold, routeCfg.MidModel)
	} else {
		fmt.Printf("  route-model: off\n")
	}
	if bl != nil {
		fmt.Printf("  budget-log: %s\n", bl.LogPath())
	}
	if effort != "" {
		fmt.Printf("  effort: %s\n", effort)
	}
	if ccrEnabled {
		fmt.Printf("  ccr: on (content-addressed recovery of pruned reads)\n")
	}
	fmt.Printf("  stats: dex proxy --stats\n")

	// Wire cost events to the SLO tracker for the current project root so
	// cost_usd SLO entries in .dex/config.yml are evaluated after each response.
	var costHook func(float64)
	if root, err := os.Getwd(); err == nil {
		tracker := slo.ForProject(root)
		costHook = tracker.RecordCostUSD
	}

	// Wire token-ratio feedback into the adaptive compression policy (#610).
	// Each response fires the hook with provider-reported token counts; the hook
	// reads the current task from ~/.cache/dex/current_task (written by the MCP
	// server on session(action=set_task)), derives intent + predicted mode, and
	// records the output/input ratio as a penalty signal.
	var feedbackHook func(outputTokens, inputTokens int64)
	if idir, err := indexDir(); err == nil {
		if root, err := os.Getwd(); err == nil {
			if p, err := proj.Resolve(root, idir); err == nil {
				policy := compress.LoadPolicy(p.CacheDir)
				feedbackHook = func(outputTokens, inputTokens int64) {
					task := readCurrentTask()
					if task == "" {
						return
					}
					predicted := compress.TaskToMode(task)
					if predicted == "" {
						return
					}
					intent := compress.IntentFromTask(task)
					mode := policy.ChooseMode(intent, predicted)
					ratio := float64(outputTokens) / float64(inputTokens)
					policy.RecordFeedback(intent, mode, ratio)
				}
			}
		}
	}

	return proxy.Run(ctx, proxy.Options{
		Addr:         *addr,
		Upstream:     *upstream,
		Logger:       cliLogger(),
		Token:        token,
		ToolDescMode: toolDescMode,
		RouteConfig:  routeCfg,
		BudgetLog:    bl,
		CostHook:     costHook,
		Effort:       effort,
		CCREnabled:   ccrEnabled,
		FeedbackHook: feedbackHook,
	})
}

// parseModelRouteConfig builds a ModelRouteConfig from flags + env. Flags take
// precedence; env vars fill in unset values; hard-coded defaults apply last.
func parseModelRouteConfig(routeModel string, lowThr int, lowModel string, midThr int, midModel string) proxy.ModelRouteConfig {
	raw := strings.TrimSpace(routeModel)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("DEX_PROXY_ROUTE_MODEL"))
	}
	if strings.ToLower(raw) != "on" {
		return proxy.ModelRouteConfig{}
	}

	if lowThr == 0 {
		lowThr = envInt("DEX_PROXY_ROUTE_LOW_THRESHOLD", 2000)
	}
	if lowModel == "" {
		if v := strings.TrimSpace(os.Getenv("DEX_PROXY_ROUTE_LOW_MODEL")); v != "" {
			lowModel = v
		} else {
			lowModel = "claude-haiku-4-5-20251001"
		}
	}
	if midThr == 0 {
		midThr = envInt("DEX_PROXY_ROUTE_MID_THRESHOLD", 20000)
	}
	if midModel == "" {
		if v := strings.TrimSpace(os.Getenv("DEX_PROXY_ROUTE_MID_MODEL")); v != "" {
			midModel = v
		} else {
			midModel = "claude-sonnet-4-6"
		}
	}
	return proxy.ModelRouteConfig{
		Enabled:      true,
		LowThreshold: lowThr,
		LowModel:     lowModel,
		MidThreshold: midThr,
		MidModel:     midModel,
	}
}

// openBudgetLog creates the per-session budget log under ~/.cache/dex/sessions/
// and writes a current_session pointer file so the PreCompact hook can locate it.
func openBudgetLog(sessionID string) (*proxy.BudgetLog, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(cacheDir, "dex", "sessions")
	bl, err := proxy.NewBudgetLog(base, sessionID)
	if err != nil {
		return nil, err
	}
	// Write pointer so the hook can resolve the current session log.
	ptr := filepath.Join(cacheDir, "dex", "current_session")
	if mkErr := os.MkdirAll(filepath.Dir(ptr), 0o755); mkErr == nil {
		_ = os.WriteFile(ptr, []byte(sessionID), 0o644)
	}
	return bl, nil
}

// printProxyStats fetches the /stats snapshot from a running proxy and prints it.
func printProxyStats(ctx context.Context, addr, token string) error {
	snap, err := proxy.FetchStats(ctx, addr, token)
	if err != nil {
		return fmt.Errorf("could not reach proxy at %s: %w\n  (start with: dex proxy --addr %s)", addr, err, addr)
	}

	pct := snap.CompressionRatio * 100
	fmt.Fprintf(os.Stdout, "dex proxy stats  addr=%s\n", addr)
	fmt.Fprintf(os.Stdout, "  requests : %d total, %d compressed, %d routed\n", snap.RequestsTotal, snap.RequestsCompressed, snap.RequestsRouted)
	fmt.Fprintf(os.Stdout, "  tokens   : %d before → %d after  (%d saved, %.1f%%)\n",
		snap.TokensBefore, snap.TokensAfter, snap.TokensSaved, pct)
	fmt.Fprintf(os.Stdout, "  re-reads : %d files re-read after prune  (%d tokens re-fetched)\n",
		snap.ReReadsAfterStub, snap.ReReadTokens)
	fmt.Fprintf(os.Stdout, "  dup-reads: %d redundant in-window reads  (%d tokens dedupable)\n",
		snap.DupReadsInWindow, snap.DupReadTokens)
	if snap.LogPath != "" {
		fmt.Fprintf(os.Stdout, "  log      : %s\n", snap.LogPath)
	}
	if snap.SessionCostUSD > 0 || snap.InputTokens > 0 {
		fmt.Fprintf(os.Stdout, "  cost     : $%.4f  (input %dk, output %dk, cache-read %dk, cache-write %dk)\n",
			snap.SessionCostUSD,
			snap.InputTokens/1000, snap.OutputTokens/1000,
			snap.CacheReadTokens/1000, snap.CacheWriteTokens/1000)
	}
	fmt.Fprintln(os.Stdout)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(snap)
}

// readCurrentTask returns the task last set via session(action=set_task), or ""
// if the file is absent or unreadable. Written by writeCurrentTask in the MCP
// server whenever the agent updates its working task description.
func readCurrentTask() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, "dex", "current_task"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
