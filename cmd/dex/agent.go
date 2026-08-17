package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/store"
)

// cmdAgent dispatches `dex agent <announce|post|read|list>` — CLI access to the
// multi-agent coordination bus (the `agents` / `agent_messages` tables). This is
// the real write path that promotes the #168 spike's throwaway beacon: a
// concurrent agent posts a finding here, and a peer's `ask()` folds it into the
// evidence pack via vector recall (#180, swarm-context-spine S2).
//
// Identity is per-process: DEX_AGENT_ID if set (the cross-process anchor so a
// `dex agent post` spawned via `act` shares the MCP server's handle), else a
// random id minted at startup. See agent_identity.go.
func cmdAgent(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("agent needs a subcommand: announce | post | read | list")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "announce":
		return cmdAgentAnnounce(ctx, rest)
	case "post":
		return cmdAgentPost(ctx, rest)
	case "read":
		return cmdAgentRead(ctx, rest)
	case "list":
		return cmdAgentList(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex agent announce [<path>] [--role R]              register/refresh this agent on the bus
  dex agent post [<path>] --category finding <body>   post a message (findings are embedded for peer recall)
  dex agent read [<path>] [--query Q] [--category C]   recall bus messages (vector recall; --any for FTS-OR)
  dex agent list [<path>]                             list registered agents

  identity: DEX_AGENT_ID (else a per-process random id) + DEX_AGENT_ROLE`)
		return nil
	default:
		return fmt.Errorf("unknown agent subcommand: %s (have: announce, post, read, list)", sub)
	}
}

func cmdAgentAnnounce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent announce", flag.ContinueOnError)
	setHelp(fs,
		"Register or refresh this agent on the coordination bus.",
		"dex agent announce [<path>] [--role R]",
		`dex agent announce --role reviewer`,
	)
	role := fs.String("role", "", "human-legible role (overrides DEX_AGENT_ROLE)")
	format := fs.String("format", "text", "output format: text|json")
	projectRoot := registerProjectFlag(fs)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, _ := projectFromFlag(*projectRoot, fs.Args())
	id, r := agentIdentity()
	if *role != "" {
		r = *role
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.AgentAnnounce(ctx, id, r); err != nil {
		return err
	}
	if *format == "json" {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Status string `json:"status"`
			ID     string `json:"agent_id"`
			Role   string `json:"role,omitempty"`
		}{"ok", id, r})
	}
	fmt.Printf("✓ announced %s%s\n", id, roleSuffix(r))
	return nil
}

func cmdAgentPost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent post", flag.ContinueOnError)
	setHelp(fs,
		"Post a message to the coordination bus (findings are embedded for peer recall).",
		"dex agent post [<path>] --category finding <body...>",
		`dex agent post --category finding "assemble bounds output to the client tool-result cap (#164)"`,
		`dex agent post --topic build --category status "ci-fast green on feat/180"`,
	)
	topic := fs.String("topic", "", "optional topic to group messages")
	category := fs.String("category", "finding", "message category (finding = embedded + folded into peer ask())")
	format := fs.String("format", "text", "output format: text|json")
	projectRoot := registerProjectFlag(fs)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := projectFromFlag(*projectRoot, fs.Args())
	body := strings.TrimSpace(strings.Join(rest, " "))
	if body == "" {
		return fmt.Errorf("agent post needs a <body>")
	}
	id, role := agentIdentity()
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Keep the agent registered so `dex agent list` and provenance roles resolve.
	_ = st.AgentAnnounce(ctx, id, role)

	// Embed the body so a peer's natural-language ask() recalls this terse
	// finding by meaning, not shared keywords. Best-effort: no embedder (or an
	// embed error) leaves it FTS-only, matching the DEX_EMBED_ENGINE=none path.
	var vec []float32
	if em := newEmbedClient(st.EmbedModel()); em != nil {
		if vecs, eerr := em.Embed(ctx, []string{body}); eerr == nil && len(vecs) > 0 {
			vec = vecs[0]
		}
	}
	msgID, err := st.AgentPostVec(ctx, id, *topic, *category, body, vec)
	if err != nil {
		return err
	}
	if *format == "json" {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Status   string `json:"status"`
			ID       int64  `json:"id"`
			AgentID  string `json:"agent_id"`
			Category string `json:"category"`
			Embedded bool   `json:"embedded"`
		}{"ok", msgID, id, *category, vec != nil})
	}
	embedTag := ""
	if vec != nil {
		embedTag = " (embedded)"
	}
	fmt.Printf("✓ posted #%d [%s] as %s%s\n", msgID, *category, id, embedTag)
	return nil
}

func cmdAgentRead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent read", flag.ContinueOnError)
	setHelp(fs,
		"Recall bus messages by vector similarity (or --any for FTS-OR).",
		"dex agent read [<path>] [--query Q] [--category C]",
		`dex agent read --query "how is the pack bounded?" --category finding`,
		`dex agent read --any --query "overflow cap" --category finding`,
	)
	topic := fs.String("topic", "", "filter to a topic")
	category := fs.String("category", "", "filter to a category (empty = any)")
	query := fs.String("query", "", "recall query (vector recall unless --any)")
	anyMatch := fs.Bool("any", false, "use FTS OR-matching instead of vector recall (no-embedder fallback)")
	k := fs.Int("k", 10, "max messages to return")
	minSim := fs.Float64("min-sim", 0.2, "vector similarity floor (0–1)")
	format := fs.String("format", "text", "output format: text|json")
	projectRoot := registerProjectFlag(fs)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, _ := projectFromFlag(*projectRoot, fs.Args())
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var msgs []store.AgentMessage
	if *query != "" && !*anyMatch {
		if em := newEmbedClient(st.EmbedModel()); em != nil {
			if vecs, eerr := em.Embed(ctx, []string{*query}); eerr == nil && len(vecs) > 0 {
				msgs, err = st.AgentQueryVec(ctx, vecs[0], *category, *k, *minSim)
				if err != nil {
					return err
				}
			}
		}
	}
	if msgs == nil { // no query, no embedder, or empty vector recall → FTS fallback
		if *anyMatch {
			msgs, err = st.AgentReadAny(ctx, *topic, *category, *query, 0, *k)
		} else {
			msgs, err = st.AgentRead(ctx, *topic, *category, *query, 0, *k)
		}
		if err != nil {
			return err
		}
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(msgs)
	}
	if len(msgs) == 0 {
		fmt.Println("no bus messages match")
		return nil
	}
	for _, m := range msgs {
		fmt.Printf("#%-4d [%s] %s%s: %s\n", m.ID, m.Category, m.AgentID, roleSuffix(m.Role), m.Body)
	}
	return nil
}

func cmdAgentList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	setHelp(fs,
		"List agents registered on the coordination bus.",
		"dex agent list [<path>]",
		`dex agent list`,
	)
	format := fs.String("format", "text", "output format: text|json")
	projectRoot := registerProjectFlag(fs)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, _ := projectFromFlag(*projectRoot, fs.Args())
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	agents, err := st.AgentList(ctx)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(agents)
	}
	if len(agents) == 0 {
		fmt.Println("no agents registered")
		return nil
	}
	for _, a := range agents {
		fmt.Printf("%s%s  last-seen %s\n", a.ID, roleSuffix(a.Role), a.LastSeenAt.Format(time.RFC3339))
	}
	return nil
}

func roleSuffix(role string) string {
	if role == "" {
		return ""
	}
	return " (" + role + ")"
}
