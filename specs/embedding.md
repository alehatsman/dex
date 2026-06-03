---
id: embedding
status: draft
owners: [aleh]
covers:
  - "internal/embed/**"
---
# Embedding

## Intent

The embedding client is dex's bridge to whatever turns text into vectors. It
speaks the OpenAI-compatible `/v1/embeddings` protocol so any conforming
backend — vLLM, TEI's compat shim, Ollama, llama.cpp-server — is
interchangeable behind one provider-agnostic contract: hand it a slice of
strings, get back one float32 vector per input, in input order. The two
properties consumers depend on are *order-preserving fan-out* (vectors line up
with inputs even under batching and concurrency) and a *distinct unreachable
signal* — a transport failure is reported differently from a bad answer, so a
consumer (the indexer, the `ask` router, the MCP server) can fall back to grep
rather than trust or persist a broken result. This spec covers the embedding
client itself; producing and storing an index from it is the indexing/storage
specs' concern.

## Behavior

- WHEN given a slice of inputs, the client returns exactly one vector per input,
  in the same order as the inputs, regardless of how the inputs were batched or
  dispatched.
- WHEN there are more inputs than the configured batch size, the client splits
  them into batches of at most that size and sends one `/v1/embeddings` request
  per batch.
- WHERE concurrency is configured above one, multiple batches are in flight at
  once (bounded by the concurrency limit) to overlap network round-trips with
  backend work; the result ordering is unaffected because each batch writes its
  own slots in the output.
- WHERE concurrency is one or less, batches are dispatched sequentially, skipping
  the goroutine/group overhead.
- WHEN inputs is empty, the client returns no vectors and makes no request.
- WHERE construction omits a batch size, concurrency, or timeout, the client
  applies safe defaults (a non-zero batch, sequential dispatch, a finite
  timeout) and sizes its HTTP connection pool to the configured concurrency so
  parallel dispatch isn't throttled by idle-connection limits.
- WHERE each request is built, the client sends the configured model name and the
  batch's inputs to `/v1/embeddings`, mapping each returned `data[i].embedding`
  back to the slot named by its response `index`.
- IF the endpoint cannot be reached at the transport layer, the client returns an
  error wrapping `ErrUnreachable`, distinct from any non-2xx HTTP status or
  server-error payload, so a consumer can detect the unreachable condition with
  `errors.Is` and fall back to grep.
- IF the backend returns a non-2xx status, an `error` payload, an unparseable
  body, a count of vectors that doesn't match the batch size, or an
  out-of-range response index, the client returns a descriptive (non-unreachable)
  error and no partial vectors.
- WHEN a reachability check is requested, the client issues a single one-input
  embed and reports success, `ErrUnreachable` on transport failure, or the
  server error otherwise.
- WHERE a consumer needs to report or key on the backend, the client exposes its
  endpoint and model name.

## Non-goals

- The chat/generation client that composes answers from retrieved context
  (**ask**) is a sibling of this client, not part of it; it shares the same
  OpenAI-compatible shape and its own unreachable signal but lives separately.
- Walking, chunking, embedding-orchestration, and persistence of an index built
  on these vectors belong to **indexing** (which consumes this client) and
  **storage** (the on-disk vector format); this spec is the client contract
  only, even though indexing also lists `internal/embed` as a consumer.
- Querying the stored vectors — embedding the query and scoring chunks
  (**semantic-search**) — is downstream and out of scope here.
- Vector dimensionality is whatever the configured model emits; dex passes
  vectors through verbatim and does not impose, validate, or convert a fixed
  dimension at this layer (dimension consistency is enforced where vectors are
  stored — **storage**).

## Checklist

- [x] Returns exactly one vector per input, in input order, under both sequential and concurrent dispatch.
- [x] Splits inputs into batches of at most the configured size, one request per batch.
- [x] Bounds concurrent in-flight batches by the configured limit and sizes the HTTP pool to match.
- [x] Empty input returns no vectors and issues no request.
- [x] Construction applies safe defaults for batch/concurrency/timeout.
- [x] Transport failures wrap `ErrUnreachable`, distinct from non-2xx / server-error / count-mismatch / bad-index errors.
- [x] No partial vectors are returned on any error.
- [x] Health check does a single one-input embed and maps failures to unreachable vs server error.
- [x] Exposes endpoint and model name for reporting/keying.
- [ ] Verified against the code by the verify workflow (flip to `living`)
