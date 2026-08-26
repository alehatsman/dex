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

	"github.com/alehatsman/dex/internal/store"
)

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
	projectRoot := registerProjectFlag(fs)
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
	path, rest := projectFromFlag(*projectRoot, fs.Args())
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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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

func cmdKnowledgeRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notes rm", flag.ContinueOnError)
	setHelp(fs,
		"Delete a project fact by id.",
		"dex notes rm [<path>] <id>",
		`dex notes rm 7`,
	)
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
