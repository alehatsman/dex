# Tuning knobs

The five env vars listed in the main [README](../README.md#environment)
are enough to run dex. This page collects the rest — knobs you
only need when you're tuning latency, capping RAM, or wiring optional
features (rerank, compress, draft).

`dex env --all --doc` prints the same list with the *currently
active* values and where each came from (`env` / `default` /
`unset` / `disabled`), which is usually the fastest way to debug "why
is rerank off."

## Optional features

These are unset by default — set the URL to enable the feature.

| Variable                  | Default | What it does                                                                 |
| ------------------------- | ------- | ---------------------------------------------------------------------------- |
| `DEX_RERANK_URL`     | unset   | Cross-encoder rerank server. Reorders the fused candidate pool before truncating to `k`. Big quality lift on conceptual queries, ~100 ms latency. |
| `DEX_COMPRESS_URL`   | unset   | Context-compression `/v1/chat/completions` server. Distils RAG chunks before generation (4–5× token savings). |
| `DEX_DRAFT_URL`      | unset   | Speculative-draft `/v1/chat/completions` server. Pre-generates code so the main chat model refines instead of writing from scratch (3–10× generation token savings). |

## Rerank tuning

Only relevant when `DEX_RERANK_URL` is set.

| Variable                       | Default                     | Notes                                                                                              |
| ------------------------------ | --------------------------- | -------------------------------------------------------------------------------------------------- |
| `DEX_RERANK_STYLE`        | `cohere`                    | `cohere` for Cohere-shape `/rerank` (TEI, Infinity, vLLM cross-encoder); `chat` for decoder-style via `/v1/chat/completions` + logprobs (e.g. Qwen3-Reranker-4B on vLLM). |
| `DEX_RERANK_MODEL`        | `BAAI/bge-reranker-v2-m3`   | Model name forwarded to the reranker.                                                              |
| `DEX_RERANK_POOL`         | `40`                        | Fused candidates fed to the reranker. Clamped to `[1, 100]`. Larger pool = better recall, slower. |
| `DEX_RERANK_TIMEOUT`      | `5s`                        | HTTP timeout per rerank call.                                                                      |
| `DEX_RERANK_CONCURRENCY`  | `4`                         | Parallel scoring goroutines (`chat` style only). Try 8–16 on a dedicated GPU. Ignored for `cohere`. |
| `DEX_DISABLE_RERANK`      | unset                       | Set `1` to short-circuit rerank even when URL is set. For A/B comparison.                          |

## Compress / draft model overrides

Each defaults to `DEX_CHAT_MODEL` — set explicitly when the
compress/draft endpoint serves a different model.

| Variable                  | Default                  | Notes                                              |
| ------------------------- | ------------------------ | -------------------------------------------------- |
| `DEX_COMPRESS_MODEL` | value of `*_CHAT_MODEL`  | Model for the compress leg.                        |
| `DEX_COMPRESS_TIMEOUT` | `30s`                  | HTTP timeout per compress call.                    |
| `DEX_DRAFT_MODEL`    | value of `*_CHAT_MODEL`  | Model for the draft leg.                           |
| `DEX_DRAFT_TIMEOUT`  | `120s`                   | HTTP timeout per draft call.                       |

## Timeouts & batching

Defaults are sized for local Ollama / vLLM. Bump these only if you see
spurious timeouts (`context deadline exceeded`) or want to push
batch throughput.

| Variable                  | Default | Notes                                       |
| ------------------------- | ------- | ------------------------------------------- |
| `DEX_EMBED_TIMEOUT`  | `60s`   | HTTP timeout per `/v1/embeddings` call.     |
| `DEX_EMBED_BATCH`    | `32`    | Max chunks per `/v1/embeddings` call.       |
| `DEX_CHAT_TIMEOUT`   | `120s`  | HTTP timeout per `/v1/chat/completions` call. |

## Index / search behavior

| Variable                     | Default      | Notes                                                                                                                                                                     |
| ---------------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `DEX_DISABLE_BM25`      | unset        | Set `1` to skip the BM25 leg of hybrid search and rank by cosine similarity alone.                                                                                        |
| `DEX_MAX_HITS_PER_FILE` | unset (no cap) | Positive integer caps how many search hits come from a single file. Promotes result diversity.                                                                          |
| `DEX_ALLOW_PATHS`       | unset        | Colon-separated path prefixes (`:` on POSIX, `;` on Windows) that `index`/`watch` accept even when the target isn't inside a git work tree. Entries support `~` / `$HOME`. |
| `DEX_EXPOSE_RAW_TOOLS`  | unset        | Set `1`/`on` to also register the raw MCP lanes (`search_semantic`, `search_symbol`, `graph_*`, `view_summarize`, `index_status`) alongside `ask`. Default off: the MCP server exposes `ask` only, since it composes those lanes and synthesizes the answer. CLI subcommands are unaffected. |

## Answer synthesis (the `ask` `answer` field)

When a chat endpoint is configured (`DEX_CHAT_URL`/`DEX_CHAT_MODEL`),
`ask` synthesizes a grounded prose `answer` from the evidence bundle on
every call and returns it alongside `answer_model`. It cites `path:line`
and is bounded by `MaxTokens` (~400) with the evidence input capped at
~12 KB so the prompt fits a 4096-token local context window. Synthesis
is best-effort: if the chat leg is unreachable the `answer` is omitted
and the agent falls back to `suggested_reads` + `next_action` (status
stays `ok`). Identical re-asks within a process are served from an
in-memory cache keyed on the question + evidence, so they cost no GPU.
