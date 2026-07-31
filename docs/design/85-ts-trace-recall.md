# Design — #85 Trustworthy TS trace/impact

Status: **implemented** · scope: name-based recall boost + honest trust signal.

## Problem

On a TypeScript-heavy repo `trace`/`impact` is the highest-differentiation dex
lane (grep can't do call-graph / blast-radius) but was *distrusted and
discarded*: TS edges are name-based (tree-sitter), recall is incomplete, and an
agent that can't trust a result pays for the call **and** re-greps by hand.

The issue proposed two paths:

1. **Full type resolution** via a TS compiler / language server (ts-morph,
   tsserver, LSP). This is the Tier-2 lane already framed in
   `docs/design/604-lsp-symbol-queries.md` — a dex-spawned, pooled LSP
   subprocess per project+lang. It is a large, new-runtime dependency and is
   **out of scope here** (constitution: minimal deps, boring tech).
2. **Failing full resolution, a confidence/recall flag** so the agent knows how
   much to trust a result and when to fall back to grep.

Path 2 was already delivered for `trace --dir callers/callees` by #727
(`Recall="partial"` + a grep sweep into `grep_hits` + a caveat hint). Two gaps
remained, addressed here.

## Change 1 — Impact recall parity (`internal/mcp/verbs.go`)

`trace --dir impact` had **none** of the #727 machinery: a non-empty TS
blast-radius was returned with no trust signal at all — the exact
"used, distrusted, discarded" failure.

`traceVerb`'s impact case now sets `Recall="partial"` and appends a caveat hint
when the resolved target is non-Go and the blast radius is non-empty
(`hasNonGoTarget` + `Total > 0`). No grep sweep is run for impact: a single
bare-symbol grep can't reconstruct a *transitive* radius, so approximating it
would over-claim. The honest signal is the flag + a hint pointing at grep for
the edges that matter. The empty case keeps its existing `nameBasedEmptyHint`
caveat.

## Change 2 — TS DI / adapter dispatch (`internal/graph`)

The resolver already binds bare / `new Foo()` / `ns.member()` / same-class
`this.method()` / default-export calls. The dominant unresolved pattern in TS
backends (NestJS / Angular / hand-rolled DI) is **constructor-injected
dependencies**:

```ts
import { AuthService } from './auth';
class UserService {
  constructor(private readonly auth: AuthService) {}
  private cache = new Cache();
  protected repo: UserRepo;
  doLogin() { this.auth.login(); this.cache.get(); this.repo.find(); }
}
```

`this.auth.login()` flattens to `["this","auth","login"]` (a length-3 `self`
chain) and was **dropped** — the old `self` case only handled `this.method()`.

The fix stays name-based, no type checker:

- **Field-type table.** During class processing, `collectClassFieldTypes` walks
  the `class_body` and records `field → type-name` from three shapes:
  1. constructor **parameter properties** — `required_parameter` carrying an
     `accessibility_modifier` / `readonly` (`private auth: AuthService`);
  2. **field declarations** with a type annotation (`protected repo: UserRepo`);
  3. **fields initialized by construction** (`private cache = new Cache()`).
  Only a simple leading `type_identifier` is taken (the container of a generic
  `Repository<User>` counts; unions / qualified / predefined types are skipped —
  they resolve to nothing, never a wrong edge).

- **Length-3 `self` resolution.** `this.<field>.<method>()` looks up the field's
  type, then resolves `<type>.<method>` exactly like a bare call —
  same-file class **or** imported class (`import { AuthService } from './auth'`),
  via `resolveTypeMethod`. `this.<method>()` (length-2) is unchanged.

### Deliberately out of scope

- **Interface-typed DI** (`constructor(private repo: IUserRepo)` where a class
  `implements IUserRepo`). Requires heritage (`implements`) parsing +
  `EdgeImplements` emission + trace-side interface-dispatch expansion (as Go
  has). Filed as a follow-up.
- **Local-variable construction typing** (`const x = new Foo(); x.m()`) — needs
  per-function-body scope tracking. Follow-up.
- **`super.method()`**, class-field arrow methods as nodes. Follow-up.

## Contract / invariants

- Precision preserved: a resolved edge points at a real method node or nothing.
  No speculative edges — a missing type or an unresolvable type yields no edge,
  same as before.
- `metadata.provenance = "sitter"` is unchanged; these are still name-based
  edges. The `Recall="partial"` signal on trace remains the truth-in-advertising
  layer.
