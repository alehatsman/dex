package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/store"
)

// cmdKnowledge dispatches `dex notes <add|list|delete|gc>` — CLI access to the
// per-project knowledge store, which was previously reachable only through the
// `ctx_knowledge` MCP tool. This lets scripts, CI steps, and commit hooks
// record and inspect project facts without an MCP session. The verbs match the
// MCP `notes` tool's action names (#576); `query`/`rm` stay as back-compat
// aliases for `list`/`delete`.
func cmdKnowledge(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("notes needs a subcommand: add | list | delete | review | pin | unpin | gc | relate | relations")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return cmdKnowledgeAdd(ctx, rest)
	case "list", "query":
		return cmdKnowledgeQuery(ctx, rest)
	case "delete", "rm":
		return cmdKnowledgeRm(ctx, rest)
	case "review":
		return cmdKnowledgeReview(ctx, rest)
	case "pin":
		return cmdKnowledgePin(ctx, rest, true)
	case "unpin":
		return cmdKnowledgePin(ctx, rest, false)
	case "gc":
		return cmdKnowledgeGC(ctx, rest)
	case "relate":
		return cmdKnowledgeRelate(ctx, rest)
	case "relations":
		return cmdKnowledgeRelations(ctx, rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, `usage:
  dex notes add [<path>] --archetype A --confidence c <body...>   store a fact
  dex notes list [<path>] [--k N]                                 top-k facts by salience
  dex notes delete [<path>] <id>                                  delete a fact by id
  dex notes review [<path>]                                       suggest merges/overlaps/stale (read-only)
  dex notes pin [<path>] <id>                                     mark a fact permanent (no decay/evict)
  dex notes unpin [<path>] <id>                                   clear the pinned flag
  dex notes gc [<path>]                                           decay + consolidate + evict
  dex notes relate [<path>] --from <id> --to <id> --kind <kind>   create/reinforce a typed edge
  dex notes relations [<path>] --id <id>                          list edges for a fact
  dex notes relations [<path>] --diagram                          Mermaid graph of all edges

  (query/rm are accepted as aliases for list/delete)`)
		return nil
	default:
		return fmt.Errorf("unknown notes subcommand: %s (have: add, list, delete, review, pin, unpin, gc, relate, relations)", sub)
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

// cmdKnowledgeReview prints advisory cleanup proposals (merge / overlap / stale)
// without mutating the store (#633). The agent reads the output and decides —
// dex never auto-applies these.
func cmdKnowledgeReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes review", flag.ContinueOnError)
	setHelp(fs,
		"Suggest knowledge-store cleanups (merge/overlap/stale) — read-only.",
		"dex notes review [flags] [<path>]",
		`dex notes review`,
		`dex notes review --format json`,
	)
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) > 0 {
		return fmt.Errorf("notes review takes no positional args besides an optional <path>")
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	res, err := st.KnowledgeReview(ctx)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	if res.Total == 0 {
		fmt.Println("✓ nothing to review — the knowledge store is tidy")
		return nil
	}
	printProposals := func(title string, ps []store.ReviewProposal) {
		if len(ps) == 0 {
			return
		}
		fmt.Printf("\n%s (%d):\n", title, len(ps))
		for _, p := range ps {
			ids := make([]string, 0, len(p.IDs))
			for _, id := range p.IDs {
				ids = append(ids, fmt.Sprintf("#%d", id))
			}
			fmt.Printf("  %s — %s\n", strings.Join(ids, " ↔ "), p.Reason)
		}
	}
	printProposals("merge", res.Merge)
	printProposals("overlap", res.Overlap)
	printProposals("stale", res.Stale)
	fmt.Printf("\n%d proposal(s). Apply with `dex notes delete|add|pin` — nothing was changed.\n", res.Total)
	return nil
}

// cmdKnowledgePin sets or clears the pinned flag on a fact (#633). A pinned fact
// is exempt from decay, eviction, and staleness proposals.
func cmdKnowledgePin(ctx context.Context, args []string, pin bool) error {
	verb := "pin"
	if !pin {
		verb = "unpin"
	}
	fs := flag.NewFlagSet("notes "+verb, flag.ContinueOnError)
	setHelp(fs,
		"Set or clear the permanent (pinned) flag on a fact.",
		"dex notes "+verb+" [<path>] <id>",
		"dex notes "+verb+" 7",
	)
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) != 1 {
		return fmt.Errorf("notes %s needs exactly one <id>", verb)
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

	if err := st.KnowledgeSetPinned(ctx, id, pin); err != nil {
		return err
	}
	if pin {
		fmt.Printf("✓ pinned fact #%d (exempt from decay/eviction)\n", id)
	} else {
		fmt.Printf("✓ unpinned fact #%d\n", id)
	}
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
		`dex notes add --archetype Hypothesis --valid-until 2026-12-01 "might be memory leak in watcher"`,
		`dex notes add --archetype ReviewFinding --scope internal/mcp/server.go "[god-object] registerTools is 442 lines; decomposition in flight (#87)"`,
		`dex notes add --supersedes 12 --archetype Architecture "revised: uses layered architecture"`,
	)
	archetype := fs.String("archetype", "Fact", "fact archetype: Architecture|Gotcha|Decision|Convention|Dependency|Pattern|Fact|ReviewFinding|Observation|Hypothesis|Inference|VerifiedFact")
	confidence := fs.Float64("confidence", 0, "confidence in (0,1] (default 0.8 when unset)")
	scope := fs.String("scope", "", "bind to a file glob/path/package (e.g. 'internal/mcp/*_test.go'); file verbs surface it on touch (#645)")
	format := fs.String("format", "text", "output format: text|json")
	supersedes := fs.Int64("supersedes", 0, "id of the fact this note replaces — marks old fact inactive immediately (#606)")
	validUntil := fs.String("valid-until", "", "expiry date YYYY-MM-DD; fact is excluded from recall after this date (#618)")
	evidence := fs.Bool("evidence", false, "mark as derived from code inspection (halves decay rate, #618)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	// Validate --archetype against the known set, normalising case so the
	// correct salience weight is applied — an unknown archetype is rejected
	// rather than silently stored with the default weight (#520).
	canonArch, ok := canonicalArchetype(*archetype)
	if !ok {
		return fmt.Errorf("invalid --archetype=%q (want one of: Architecture, Gotcha, Decision, Convention, Dependency, Pattern, Fact, ReviewFinding, Observation, Hypothesis, Inference, VerifiedFact)", *archetype)
	}
	*archetype = canonArch
	// --confidence is a (0,1] value; 0 is the unset sentinel that defers to the
	// store default (0.8). Reject an explicitly-supplied out-of-range value
	// instead of silently coercing it (negative→default, >1→clamp) (#520).
	confSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "confidence" {
			confSet = true
		}
	})
	if confSet && (*confidence <= 0 || *confidence > 1) {
		return fmt.Errorf("invalid --confidence=%g (want a value in (0,1])", *confidence)
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

	opts := store.KnowledgeAddOpts{
		Scope:    *scope,
		Evidence: *evidence,
	}
	if *validUntil != "" {
		t, err := parseKnowledgeDate(*validUntil)
		if err != nil {
			return fmt.Errorf("--valid-until: %v", err)
		}
		opts.ValidUntil = t
	}

	// Near-duplicate scan before insert so the new note doesn't match itself (#606).
	similar, _ := st.KnowledgeSimilar(ctx, body, 0.5, 3)
	var rev int
	if *supersedes > 0 {
		rev, err = st.KnowledgeSupersede(ctx, *supersedes, *archetype, body, *confidence, opts)
	} else {
		rev, err = st.KnowledgeAddFull(ctx, *archetype, body, *confidence, opts)
	}
	if err != nil {
		return err
	}
	if *format == "json" {
		type simOut struct {
			ID         int64   `json:"id"`
			Archetype  string  `json:"archetype"`
			Body       string  `json:"body"`
			Similarity float64 `json:"similarity"`
		}
		sims := make([]simOut, 0, len(similar))
		for _, sf := range similar {
			sims = append(sims, simOut{sf.ID, sf.Archetype, sf.Body, sf.Similarity})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Status        string   `json:"status"`
			Archetype     string   `json:"archetype"`
			Body          string   `json:"body"`
			RevisionCount int      `json:"revision_count"`
			Similar       []simOut `json:"similar,omitempty"`
		}{"ok", *archetype, body, rev, sims})
	}
	if *supersedes > 0 {
		fmt.Printf("✓ stored [%s] %s — fact #%d marked inactive\n", *archetype, body, *supersedes)
	} else if rev == 0 {
		fmt.Printf("✓ stored [%s] %s\n", *archetype, body)
	} else {
		fmt.Printf("✓ updated [%s] %s (revision %d)\n", *archetype, body, rev)
	}
	for _, sf := range similar {
		fmt.Printf("  ⚠ similar (%.0f%%) #%d [%s] %s\n", sf.Similarity*100, sf.ID, sf.Archetype, sf.Body)
	}
	return nil
}

// parseKnowledgeDate parses an RFC3339 or YYYY-MM-DD date string.
func parseKnowledgeDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339, got %q", s)
	}
	return t, nil
}

func cmdKnowledgeRelate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes relate", flag.ContinueOnError)
	setHelp(fs,
		"Create or reinforce a typed edge between two facts (#621).",
		"dex notes relate [<path>] --from <id> --to <id> --kind <kind>",
		`dex notes relate --from 3 --to 7 --kind DependsOn`,
		`dex notes relate --from 5 --to 2 --kind Supersedes`,
	)
	fromID := fs.Int64("from", 0, "source fact id")
	toID := fs.Int64("to", 0, "target fact id")
	kind := fs.String("kind", "", "edge kind: DependsOn|RelatedTo|Supports|Contradicts|Supersedes")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) > 0 {
		return fmt.Errorf("notes relate takes no positional args (got %q)", strings.Join(rest, " "))
	}
	if *fromID <= 0 || *toID <= 0 {
		return fmt.Errorf("notes relate: --from and --to are required")
	}
	if *kind == "" {
		return fmt.Errorf("notes relate: --kind is required (DependsOn|RelatedTo|Supports|Contradicts|Supersedes)")
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.KnowledgeRelate(ctx, *fromID, *toID, *kind); err != nil {
		return err
	}
	fmt.Printf("✓ edge #%d -[%s]→ #%d recorded\n", *fromID, *kind, *toID)
	return nil
}

func cmdKnowledgeRelations(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes relations", flag.ContinueOnError)
	setHelp(fs,
		"List edges for a fact, or emit a Mermaid diagram of all edges (#621).",
		"dex notes relations [<path>] --id <id>",
		`dex notes relations --id 7`,
		`dex notes relations --diagram`,
	)
	id := fs.Int64("id", 0, "fact id to list edges for")
	diagram := fs.Bool("diagram", false, "emit a Mermaid graph of all edges")
	minStr := fs.Float64("min-strength", 0.0, "minimum edge strength to include in diagram (0.0 = all)")
	format := fs.String("format", "text", "output format: text|json")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	path, rest := splitProjectArg(fs.Args())
	if len(rest) > 0 {
		return fmt.Errorf("notes relations takes no positional args (got %q)", strings.Join(rest, " "))
	}
	st, _, err := openProjectStore(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if *diagram {
		mermaid, err := st.KnowledgeRelationDiagram(ctx, *minStr)
		if err != nil {
			return err
		}
		fmt.Println(mermaid)
		return nil
	}
	if *id <= 0 {
		return fmt.Errorf("notes relations: --id is required (or use --diagram for the full graph)")
	}
	rels, err := st.KnowledgeRelations(ctx, *id)
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rels)
	}
	if len(rels) == 0 {
		fmt.Printf("fact #%d has no relations yet — add one with `dex notes relate`\n", *id)
		return nil
	}
	for _, r := range rels {
		dir := "→"
		peer := r.ToID
		if r.FromID != *id {
			dir = "←"
			peer = r.FromID
		}
		fmt.Printf("  #%d %s[%s] #%d  strength=%.2f count=%d\n", *id, dir, r.Kind, peer, r.Strength, r.Count)
	}
	return nil
}

// listFacts returns the notes whose scope binds `scope` when it is set (#653) —
// what would surface on touching that path — otherwise the top-k by salience.
func listFacts(ctx context.Context, st *store.Store, scope string, k int) ([]store.KnowledgeFact, error) {
	if scope != "" {
		return st.KnowledgeByScope(ctx, scope, k)
	}
	return st.KnowledgeQuery(ctx, k)
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
	scope := fs.String("scope", "", "filter to notes whose scope binds this path (what surfaces on touching it, #653)")
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

	facts, err := listFacts(ctx, st, *scope, *k)
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
		scopeTag := ""
		if f.Scope != "" {
			scopeTag = fmt.Sprintf(" {scope:%s}", f.Scope)
		}
		fmt.Printf("#%-4d [%-12s] conf=%.2f hits=%d  %s%s\n", f.ID, f.Archetype, f.Confidence, f.HitCount, f.Body, scopeTag)
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
