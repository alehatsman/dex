package main

import (
	"context"
	"fmt"
	"os"

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
