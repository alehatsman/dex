package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alehatsman/dex/internal/store"
)

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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
	path, rest, err := parseNotesArgs(fs, args)
	if err != nil {
		return err
	}
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
