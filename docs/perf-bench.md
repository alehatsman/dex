# dex bench perf — what it measures and what it does NOT

## What it measures

`dex bench perf` runs local-compute paths over synthetic data (no GPU, no
network) and reports p50/p95/p99 latencies and storage footprint.

### Compress pass latency
Timing of each compress pass over a ~800-line Go code block. Shows which passes
are latency-safe for use in the hot redirect path vs. the background proxy path.

### KNN vector search scaling curves
p50/p95/p99 at corpus sizes 1k / 5k / 20k / 50k / 100k (1024-dim vectors,
matching nomic-embed-text / nomic-embed-code). The scaling curve shows where
brute-force KNN crosses a latency budget — **the ANN break-even trigger for
#216**. Example: at 100k chunks the p50 is ~85ms; at 1k it is ~900µs. When the
target use-case cannot tolerate the large-corpus p50, ANN is justified.

### BM25/FTS5 search
p50/p95/p99 for a typical multi-term text query against 5k chunks. Shows that
BM25 is ~100x faster than KNN at the same corpus size — relevant to the
DisableBM25 fallback path and the weight the BM25 leg should carry in RRF.

### Storage footprint
Index size in bytes at 1k / 5k / 20k chunks. Documents the per-chunk storage
cost at 1024-dim vectors (float32 = 4 bytes/dim → ~4 KiB/chunk of raw vectors;
sqlite overhead on top). **Justifies int8 quantisation (#215, 4× reduction) and
Matryoshka dim truncation (#249, done).**

Baseline committed at `benchmark/perf/baseline.json`. Regression gate: p99 must
not exceed 4× baseline (p99 is noisy on shared boxes; 4× catches algorithmic
regressions while tolerating OS scheduling variance). Refresh with:

```
dex bench perf --output json > benchmark/perf/baseline.json
```

## What it does NOT measure

**Graph expansion latency.**
No synthetic graph builder is wired up yet — the store accepts graph edges but
the bench doesn't populate them. Once a graph-populating helper exists, add a
`graph/knn+expand` timing that shows the overhead of k-hop neighbour fetches
on top of KNN.

**Embedding throughput.**
Embed RTT is GPU/network-bound and wildly variable on a shared box. Measuring
it would make the gate flaky. The bench intentionally skips it. Report-only tier
(never gated): run `dex index . && time dex index .` and subtract filesystem
walk time.

**Rerank and chat RTT.**
Same reason — network-bound, variable. Report-only: benchmark with
`DEX_DISABLE_RERANK=0` and compare ask latency end-to-end.

**Concurrent MCP session throughput.**
The drain-lock / foreground-yield path (#17) matters under N concurrent MCP
sessions but is not yet in scope here. Add a goroutine-parallel bench when the
proxy (#232) is live and session concurrency becomes a bottleneck.

**Cold startup latency.**
First-query latency (watcher spawn, DB open, first embed warm-up) requires a
full subprocess invocation, not in-process timing. Measure with:
`time dex ask "warm-up query"` on a cold box.

**GPU-bound paths (report-only).**
Embedding fidelity (embed similarity pre/post int8 quantisation), rerank
accuracy delta, and ask end-to-end correctness are GPU-dependent and deferred
until there is GPU headroom.
