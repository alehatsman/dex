# dex CI image — the toolchain image dex's mgitci.yml runs on.
#
# Why dex needs its own image: the quality gate (`mooncake task ci` ->
# go-quality goq/ci -> scripts/ci/full.sh) builds, tests, lints and vuln-checks
# with `-tags sqlite_fts5`. That tag compiles cgo `mattn/go-sqlite3` (the
# chunks_fts virtual table + hybrid BM25 leg require FTS5), so the container
# needs a C compiler. moongit-ci-dev:latest carries Go + Node + the
# static-analysis toolchain + mooncake, but is deliberately cgo-free
# (CGO_ENABLED=0, no gcc) — a tag-less / cgo-less build wipes the index on
# reindex. So we derive from it and add the C toolchain, mirroring mooncake's
# ci/Dockerfile -> mooncake-ci:latest pattern.
#
# Build (from the dex repo root, after moongit-ci-dev:latest exists):
#   docker build -t dex-ci:latest ci/
FROM moongit-ci-dev:latest

# cgo toolchain for the sqlite_fts5 build:
#   - build-essential: gcc + libc headers (mattn/go-sqlite3 bundles its own
#     amalgamation but still needs a C compiler).
#   - libsqlite3-dev: system sqlite3.h, which sqlite-vec-go-bindings/cgo
#     (sqlite-vec.h -> #include "sqlite3.h") compiles against — unlike mattn's
#     package it carries no bundled header.
# CGO_ENABLED=1 so go build/test pick up the C toolchain even though the base
# image set it to 0.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential libsqlite3-dev \
 && rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=1

# Fail the build early if the cgo toolchain or a sqlite_fts5 compile is broken.
RUN gcc --version && cc --version
