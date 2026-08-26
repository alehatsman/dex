package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
