package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// cmdKnowledge dispatches `dex notes <add|query|rm>` — CLI access to the
// per-project knowledge store, which was previously reachable only through the
// `ctx_knowledge` MCP tool. This lets scripts, CI steps, and commit hooks
// record and inspect project facts without an MCP session.
func cmdKnowledge(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("notes needs a subcommand: add | query | rm")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdKnowledgeAdd(ctx, rest)
	case "query", "list":
		return cmdKnowledgeQuery(ctx, rest)
	case "rm", "delete":
		return cmdKnowledgeRm(ctx, rest)
	case "gc":
		return cmdKnowledgeGC(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex notes add [<path>] --archetype A --confidence c <body...>   store a fact
  dex notes query [<path>] [--k N]                                top-k facts by salience
  dex notes rm [<path>] <id>                                      delete a fact by id
  dex notes gc [<path>]                                           decay + consolidate + evict`)
		return nil
	default:
		return fmt.Errorf("unknown notes subcommand: %s (have: add, query, rm, gc)", sub)
	}
}

// cmdKnowledgeGC runs the knowledge-store lifecycle pass: confidence decay,
// consolidation of near-duplicate facts, and eviction past the cap.
func cmdKnowledgeGC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes gc", flag.ContinueOnError)
	setHelp(fs,
		"Run the knowledge-store lifecycle: decay, consolidate, evict.",
		"dex notes gc [flags] [<path>]",
		`dex notes gc`,
		`dex notes gc --max-facts 500 --format json`,
	)
	maxFacts := fs.Int("max-facts", 0, "evict lowest-confidence facts beyond this cap (0 = default 1000)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) > 0 {
		return fmt.Errorf("notes gc takes no positional args besides an optional <path>")
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	res, err := st.KnowledgeGC(ctx, store.KnowledgeGCConfig{MaxFacts: *maxFacts})
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Printf("✓ gc: decayed %d, merged %d, evicted %d — %d facts remain\n",
		res.Decayed, res.Merged, res.Evicted, res.Remaining)
	return nil
}

// openProjectStore resolves the project for path and opens its store, erroring
// with a friendly hint when the project has not been indexed yet.
func openProjectStore(ctx context.Context, path string) (*store.Store, *proj.Project, error) {
	base, err := indexDir()
	if err != nil {
		return nil, nil, err
	}
	p, err := proj.Resolve(path, base)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(p.DBPath); err != nil {
		return nil, nil, fmt.Errorf("no index for %s — run `dex index %s` first", p.Root, path)
	}
	st, err := openStore(ctx, p.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return st, p, nil
}

func cmdKnowledgeAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes add", flag.ContinueOnError)
	setHelp(fs,
		"Store a project fact in the knowledge store.",
		"dex notes add [flags] [<path>] <body...>",
		`dex notes add --archetype Gotcha "store tests need -tags sqlite_fts5"`,
		`dex notes add --archetype Decision --confidence 0.9 "we use yaml.v3 for config"`,
	)
	archetype := fs.String("archetype", "Fact", "fact archetype: Architecture|Gotcha|Decision|Convention|Dependency|Pattern|Fact")
	confidence := fs.Float64("confidence", 0, "confidence in (0,1] (default 0.8 when unset)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	body := strings.TrimSpace(strings.Join(rest, " "))
	if body == "" {
		return fmt.Errorf("notes add needs a <body> (the fact text)")
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	rev, err := st.KnowledgeAdd(ctx, *archetype, body, *confidence)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Status        string `json:"status"`
			Archetype     string `json:"archetype"`
			Body          string `json:"body"`
			RevisionCount int    `json:"revision_count"`
		}{"ok", *archetype, body, rev})
	}
	if rev == 0 {
		fmt.Printf("✓ stored [%s] %s\n", *archetype, body)
	} else {
		fmt.Printf("✓ updated [%s] %s (revision %d)\n", *archetype, body, rev)
	}
	return nil
}

func cmdKnowledgeQuery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes query", flag.ContinueOnError)
	setHelp(fs,
		"List top-k project facts ordered by salience.",
		"dex notes query [flags] [<path>]",
		`dex notes query`,
		`dex notes query --k 20 --format json`,
	)
	k := fs.Int("k", 10, "max facts to return (1–50)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) > 0 {
		return fmt.Errorf("notes query takes no positional args besides an optional <path> (got %q) — semantic query is #223", strings.Join(rest, " "))
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	facts, err := st.KnowledgeQuery(ctx, *k)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(facts)
	}
	if len(facts) == 0 {
		fmt.Println("no facts stored — add some with `dex notes add`")
		return nil
	}
	for _, f := range facts {
		fmt.Printf("#%-4d [%-12s] conf=%.2f hits=%d  %s\n", f.ID, f.Archetype, f.Confidence, f.HitCount, f.Body)
	}
	return nil
}

func cmdKnowledgeRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes rm", flag.ContinueOnError)
	setHelp(fs,
		"Delete a project fact by id.",
		"dex notes rm [<path>] <id>",
		`dex notes rm 7`,
	)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		return fmt.Errorf("notes rm needs exactly one <id>")
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q: must be an integer", rest[0])
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.KnowledgeDelete(ctx, id); err != nil {
		return err
	}
	fmt.Printf("✓ deleted fact #%d\n", id)
	return nil
}
