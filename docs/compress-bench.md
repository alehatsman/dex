# dex bench compress — what it measures and what it does NOT

## What it measures

`dex bench compress` runs each compress pass over a set of representative
fixtures (Go source, test output, git diff, markdown) and reports:

| metric | definition | gate class |
|--------|-----------|-----------|
| **ratio** | `tokens_out / tokens_in` (lower = better compression) | both |
| **anchor%** | fraction of critical tokens (paths, identifiers, line numbers) surviving verbatim | both |
| **extract%** | fraction of answer spans surviving as substrings | lossy only |
| **round-trip** | lossless-pass compressed output reconstructs to original | lossless only |

**Two gate classes** (matching the issue spec #296):
- *Lossless passes* (codebook, ngram_codebook, symmap): gated on ratio +
  round-trip == original. A reconstruction failure is always a hard regression.
- *Lossy passes* (aggressive, entropy, terse, ib): gated on ratio +
  extractive-fidelity floor. A ratio win that drops fidelity below baseline by
  > 5% is a regression.

Baseline committed at `benchmark/compress/baseline.json`. Refresh with:

```
dex bench compress --output json > benchmark/compress/baseline.json
```

## Anchor-verbatim floor — strict / weak target_model (#291)

`anchor%` is a *measurement*; the strict path is the *enforcement*. Weak local
models (the `tokens.Llama` family — Qwen/DeepSeek/Llama/Mistral) hallucinate
plausible-wrong tokens when an exact path, qualified identifier, or type name is
mutated, so for those targets compression guarantees `anchor% == 100`.

`Profile.StrictAnchors()` returns true for the weak family and false for
frontier targets (Claude/GPT/Gemini). When true, the serving paths
(`file_view --mode aggressive`, `dex read`, `dex compress`) call
`compress.CompressCode(content, ext, strict=true)` →
`compress.AggressiveCompressStrict`, which extracts an `AnchorSet` (paths with
optional `:line`, dotted/`::` qualified identifiers, multi-segment PascalCase
type names) from the comment-stripped source and holds each anchor off the four
mutating passes:

- entropy line-drop never drops a line containing an anchor,
- token reductions skip rules whose source overlaps an anchor,
- the symbol map and n-gram codebook exclude entries that would rewrite or
  delete an anchor span.

The floor holds by construction across every aggressiveness level — it is the
hard floor that #163's adaptive ratio can never override. Frontier targets keep
the relaxed pipeline (symmap/n-gram handles on) unchanged.

## What it does NOT measure

**Embedding-similarity fidelity** (`--with-embed`, deferred).
Measures cosine(embed(original), embed(compressed)) — one embed call per sample.
Tells you whether the compressed form lands in the same vector neighbourhood.
Deferred: requires a live embed endpoint and a GPU; cannot run on a
GPU-pressured box without displacing foreground work. Add `--with-embed` when
GPU headroom is available (#296 opt-in tier).

**Task-success-per-token** (`--with-llm`, deferred).
The north star from #157: does the agent succeed at the task with the compressed
context? Requires a live chat endpoint. Deferred until GPU headroom exists and
the proxy (#232) is in place to give a real token-savings number. Add
`--with-llm` when both conditions hold.

**Cross-tokenizer ratio accuracy**.
Ratio is counted with the `--tokenizer` flag (default: o200k_base, matching
lean-ctx COUNTING_FAMILY). The flag exists (#292 tokenizer-gated rules need it),
but the *corpus* samples are generic — not yet tailored to expose o200k vs Qwen
BPE divergence. The per-tokenizer Pareto table (#292 instrument) emerges once
the corpus is expanded with content where rule eligibility differs by family.

**Production traffic distribution**.
The built-in corpus is small (4 samples) and synthetic. Real dex tool outputs
— especially large `ctx_shell` outputs, multi-file `file_view` results, and
long graph dumps — are not yet represented. Expand `benchcompress.BuiltinCorpus`
with real-session fixtures to close the gap.

**Composed-pass metrics**.
Current report scores each pass independently. Composition (codebook → symmap →
aggressive applied in sequence) is not yet measured. The per-pass Pareto
frontier is the correct first step (know which passes earn their keep
individually before composing).
