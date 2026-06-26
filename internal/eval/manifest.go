package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ManifestSchemaVersion is bumped whenever the set of identity fields in
// EvalManifest changes in a way that makes older manifests non-comparable.
// A mismatch is itself an incompatibility (see Incompatible).
const ManifestSchemaVersion = 2

// EvalManifest records the experiment identity of a Report: which golden
// corpus was scored, under which retrieval configuration, against which code.
// It is embedded in Report and serialized with it so a committed baseline
// carries the exact conditions that produced its metrics. Two reports are only
// comparable when their manifests are compatible (see Incompatible) — this is
// what lets `--check` reject false passes/failures such as k=10 vs k=20,
// git-history vs orphan, or BM25-only vs full dense.
//
// RepoHead and GoldenHead are recorded but are NOT identity fields: a stale
// golden set (golden HEAD != repo HEAD) is still a valid comparison and is
// gated separately (see StaleGolden), so they never make two runs
// "incompatible".
type EvalManifest struct {
	SchemaVersion  int     `json:"schema_version"`
	GoldenMode     string  `json:"golden_mode"`             // git-history | blast-radius | structural | orphan
	GoldenSHA256   string  `json:"golden_sha256,omitempty"` // hash of the golden-set file bytes (informational)
	GoldenHead     string  `json:"golden_head,omitempty"`   // HEAD the golden set was generated from
	RepoHead       string  `json:"repo_head,omitempty"`     // current repo HEAD at scoring time
	QuerySetSHA256 string  `json:"query_set_sha256"`        // hash of the query corpus (id+query+relevant), order-independent
	Lane           string  `json:"lane"`                    // full | bm25 | onnx
	EmbedModel     string  `json:"embed_model,omitempty"`   // index-recorded embedding model identity
	EmbedDim       int     `json:"embed_dim,omitempty"`     // index vector dimension
	FusionMode     string  `json:"fusion_mode"`             // rrf | linear
	FusionAlpha    float32 `json:"fusion_alpha"`            // dense-lane weight in linear mode
	GraphWeight    float32 `json:"graph_weight"`            // graph-proximity lane multiplier
	K              int     `json:"k"`                       // retrieval depth
	RerankEnabled  bool    `json:"rerank_enabled"`          // true when a reranker was wired into storeOpts
}

// Incompatible returns the identity fields that differ between m and ref in a
// way that makes the two reports non-comparable. An empty result means a
// metric comparison is meaningful. GoldenHead/RepoHead/GoldenSHA256 are
// deliberately excluded — corpus identity is QuerySetSHA256, and HEAD drift is
// handled by StaleGolden.
func (m EvalManifest) Incompatible(ref EvalManifest) []string {
	var diffs []string
	add := func(name string, now, was any) {
		if now != was {
			diffs = append(diffs, fmt.Sprintf("%s (ref=%v now=%v)", name, was, now))
		}
	}
	add("schema_version", m.SchemaVersion, ref.SchemaVersion)
	add("golden_mode", m.GoldenMode, ref.GoldenMode)
	add("query_set_sha256", m.QuerySetSHA256, ref.QuerySetSHA256)
	add("lane", m.Lane, ref.Lane)
	add("embed_model", m.EmbedModel, ref.EmbedModel)
	add("embed_dim", m.EmbedDim, ref.EmbedDim)
	add("fusion_mode", m.FusionMode, ref.FusionMode)
	add("fusion_alpha", m.FusionAlpha, ref.FusionAlpha)
	add("graph_weight", m.GraphWeight, ref.GraphWeight)
	add("k", m.K, ref.K)
	add("rerank_enabled", m.RerankEnabled, ref.RerankEnabled)
	return diffs
}

// QuerySetSHA256 hashes the query corpus of a golden set: per query its ID,
// text, and sorted relevant files, combined order-independently so the digest
// depends on the set's content, not its serialization order. Two golden sets
// with the same queries (in any order) share a digest; adding, removing, or
// editing any query changes it.
func QuerySetSHA256(gs GoldenSet) string {
	lines := make([]string, len(gs.Queries))
	for i, q := range gs.Queries {
		rel := append([]string(nil), q.RelevantFiles...)
		sort.Strings(rel)
		lines[i] = q.ID + "\x00" + q.Query + "\x00" + q.Anchor + "\x00" + strings.Join(rel, ",")
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256Hex returns the hex-encoded SHA-256 of data. Used to fingerprint the
// golden-set file bytes for the manifest.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// StaleGolden reports whether a golden set was generated against a different
// repo HEAD than the one being scored. Both heads must be known; an unknown
// (empty) head is never reported stale.
func StaleGolden(goldenHead, repoHead string) bool {
	return goldenHead != "" && repoHead != "" && goldenHead != repoHead
}

// RepoHead resolves the current HEAD commit of the git repository at root.
func RepoHead(ctx context.Context, root string) (string, error) {
	return gitOutput(ctx, root, "rev-parse", "HEAD")
}

// shortHead abbreviates a commit hash for display; empty stays "?".
func shortHead(h string) string {
	if h == "" {
		return "?"
	}
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
