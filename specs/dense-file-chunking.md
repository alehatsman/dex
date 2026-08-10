---
id: dense-file-chunking
status: validated
last_verified: 8b6370e
owners: [aleh]
covers:
  - "internal/chunk/chunk.go"
  - "internal/chunk/pack.go"
  - "internal/index/index.go"
  - "cmd/dex/main_index.go"
---
# Density-aware chunking (#131)

## Intent

The per-file chunk-density cap (`DefaultMaxChunksPerFile = 500`) exists so one
file can't flood the embedding index. Today, crossing the cap **drops the file
entirely** from chunk/embed/FTS (`internal/index/index.go` — chunks omitted,
reaped by `PruneUnseen`). That is silent, unpredictable data loss at an
arbitrary threshold.

The cap fires on **dense declaration files** — overwhelmingly generated ones.
Live evidence (`bright-frontend`, measured via the structural chunker):

| File | bytes | structural chunks | decl-size p50 / p90 / max |
|------|------:|------------------:|---------------------------|
| `Service/CMDevice.ts` | 226 KB | **1849** | 98B / 205B / 430B |
| `Entity/Device.ts` | 86 KB | 259 | 154B / 844B / 3789B |
| `Entity/Generated/Metadata.ts` | 257 KB | 219 | 1369B / 2210B / 4065B |

Two things stand out:

1. **Only `CMDevice.ts` trips the cap** (1849 > 500). It is uniformly tiny
   declarations — every chunk < 512 B (mostly `export const x = 'TOKEN'`
   one-liners; even its interfaces are small param types). Its longest run of
   ≤160 B declarations is **205**.
2. **`Device.ts` and `Metadata.ts` are under the cap** and carry genuinely large
   declarations (entity interfaces up to 3.8 KB). These we must **not** touch —
   their per-declaration precision is exactly what makes `search`/`brief` useful.

The goal, chosen deliberately: **index everything, never drop.** A dense file
should degrade to *coarser chunks*, not disappear. And it should happen
automatically off the real signal — declaration density — not off a
generated-file marker or a user-facing policy. (An earlier draft proposed
skip/pack/full policies + generated detection + `.gitattributes`; rejected as
both lossy and more machinery than the problem needs — see Alternatives.)

## Scope

**The cap stops meaning "drop" and starts meaning "pack this file."**

Pipeline change (`internal/index/index.go`, chunk slow-path), replacing the drop:

```
chunks := chunk.Chunks(...)                 // structural, unchanged
if limit > 0 && len(chunks) > limit {
    chunks = chunk.PackDense(rel, src, chunks)   // coarsen, never drop
}
```

`chunk.PackDense(relPath, src, chunks)` (new, `internal/chunk/pack.go`):

1. Sort input chunks by start byte (defensive; structural + orphan chunks
   already cover the file contiguously).
2. Walk in order, maintaining a pending run of consecutive **small**
   declarations (`len(Content) < DenseBigThreshold`).
   - A **big** declaration (`≥ DenseBigThreshold`) flushes the pending run, then
     is emitted **standalone, unchanged** — full precision preserved.
   - Small declarations accumulate; when the run's byte span would exceed
     `MaxBytes`, it is flushed as one packed chunk and a new run starts.
3. A flushed run becomes a chunk with `Content = src[run[0].startByte :
   run[n].endByte]` (includes inter-declaration gap text — nothing lost),
   `StartLine/EndLine` taken from the run's first/last member, `Kind =
   KindPacked`.
4. Never drops. If a file is all-big (e.g. 600 real interfaces), packing can't
   reduce it below the cap and that is correct — 600 meaningful declarations are
   600 meaningful vectors. The result may exceed the cap; it is still emitted in
   full.

Chunks with no byte range (`startByte==endByte==0`, the window-fallback path
used only when tree-sitter fails) are treated as big and emitted standalone, so
packing never mis-slices. In practice a file that reaches the cap was
AST-chunked and every chunk carries a byte range.

### Constants (`internal/chunk`)

- `DenseBigThreshold = 512` — a declaration ≥ this keeps its own chunk. Grounded
  in the probe: CMDevice's largest real declaration is 430 B (so all pack →
  ~56 chunks), while Device's entity interfaces are >512 B (so they'd stay
  standalone if that file ever tripped the cap). Tunable.
- `MaxBytes = 4096` — existing window budget; a packed run targets this size.

### What is removed

The generated-file policy work from the prior draft is reverted: no
`GeneratedPolicy`, no `IsGenerated`, no `gitattr.go`, no `index.generated`
config, no policy branch in the pipeline. The `MaxChunksPerFile` config knob
stays (now the pack trigger; `<= 0` disables packing → full precision always,
never dropped). `SkipMinified` / `LooksMinified` stay unchanged (check 1).

## Interfaces

- `internal/chunk/pack.go`
  - `func PackDense(relPath string, src []byte, chunks []Chunk) []Chunk`
  - `const DenseBigThreshold = 512`
  - `KindPacked = "packed"` kind constant
- `internal/index/index.go` — pipeline: replace the density drop with the
  `PackDense` call above; drop the generated-policy branch and `genAttrs`.
- `cmd/dex/main_index.go` — dry-run: mirror the pack (report `packed dense: N
  files (X→Y chunks)`), drop the `chunk-density` skip reason.

## Edge cases

- **Minified/bundled** — `LooksMinified` still fires first (check 1); those
  files never reach packing.
- **File under the cap** — untouched, byte-for-byte identical to today. This is
  the safety property: normal source and moderately-large files (Device.ts,
  Metadata.ts) keep full per-declaration precision.
- **All-big dense file** — packing keeps every big declaration standalone;
  result may stay > cap; emitted in full (never dropped).
- **Grep / file_tree / search** — a packed file has chunks, so it appears in
  `Store.FileTree` and is fully greppable, listable, and searchable (coarser
  granularity only). The `skip`-era visibility hole does not arise. (#132, the
  broader "grep/file_tree should enumerate the working tree" fix, is now
  independent of this work — no longer a dependency.)
- **Provenance** — a packed chunk's `[StartLine,EndLine]` spans its run so
  `read`/citations resolve to the right region; exact-symbol lookup stays served
  by the graph symbol index.
- **Reindex churn** — files over the cap get new chunk boundaries → new content
  hashes → re-embed once. The #121 content-addressed cache covers unchanged
  files. Files under the cap: no change, no churn.

## Validation

- **Unit** (`internal/chunk/pack_test.go`):
  - Dense synthetic file (1000 tiny decls) → `PackDense` yields
    ⌈bytes/MaxBytes⌉-order chunks, full byte coverage, monotonic
    non-overlapping spans, every declaration's text still present.
  - Mixed file (big interfaces between small runs) → big decls emitted
    standalone unchanged; only small runs coalesce.
  - Idempotence / no-op: a chunk list already under `DenseBigThreshold` count
    with all-big members returns unchanged.
- **Pipeline** (`internal/index`): file > cap is packed (≪ structural count,
  0 dropped); file < cap is unchanged; `MaxChunksPerFile <= 0` disables packing.
- **Live** (bright-frontend): reindex; assert CMDevice.ts drops from 1849 → ~56
  chunks and stays searchable (a specific `X_TOKEN` string still found by
  `grep`/`search`); assert Device.ts / Metadata.ts chunk counts unchanged;
  measure total index time and vector count vs baseline; spot-check `brief` /
  `search` quality on a device-domain query.
- **Gate:** `mooncake task ci`.

### Validation results (live, bright-frontend, 8b6370e)

- `mooncake task ci` green (build + test + vet + fmt).
- Dry-run: **6 files dense-packed**, total 31824 chunks vs 31621 baseline
  (**+203 chunks** to restore 6 previously-*dropped* generated files —
  CMDevice.ts et al. — to searchability; the old cap dropped them to 0).
- Full `dex reindex`: 3081 files / 31696 chunks in ~99 s (vector cache #121
  kept re-embed to just the 6 changed files).
- Quality: `grep getNetworkManagerConfig` now hits `CMDevice.ts:6633`;
  `search "network manager config device token"` returns `CMDevice.ts` `(packed)`
  chunks at score 0.94, ~68-line granularity. Both returned nothing from the
  index before (file was dropped).
- **Upgrade note:** incremental `dex index` skips unchanged files via the mtime
  fast-path, so previously-dropped dense files reappear only on `dex reindex`
  (or when they next change). A one-time reindex after upgrading applies packing.

## Alternatives considered

- **Skip/pack/full generated-file policy** (prior draft) — rejected: `skip`
  drops content (the thing we explicitly don't want) and, because
  `Store.FileTree` is chunk-backed, hides files from grep/file_tree/search
  (#132); `pack` as a whole-file mode dilutes big-interface vectors; and the
  whole scheme needs generated detection + `.gitattributes` + config the
  density trigger makes unnecessary.
- **Signatures-only for generated files** — rejected earlier: const tokens have
  no body, interface/type fields *are* the content, and cost is vector count
  not bytes.
- **Remove the cap entirely** — rejected: unbounded vector count on pathological
  files. Packing bounds it (~bytes/MaxBytes) without dropping.
- **Pack unconditionally (all files)** — rejected: hand-written and
  moderately-large files benefit from per-declaration precision. Trigger only
  above the cap, where a file is provably pathological.
