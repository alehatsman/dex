package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func indexDir() (string, error) {
	if v := os.Getenv("DEX_INDEX_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "dex"), nil
}

// storeOpts reads runtime tweaks from the environment so every code
// path that opens a Store sees the same configuration.
func storeOpts() store.Options {
	opts := store.Options{
		SearchOptions: store.SearchOptions{
			DisableBM25:    os.Getenv("DEX_DISABLE_BM25") == "1",
			MaxHitsPerFile: maxHitsPerFile(),
			FusionMode:     fusionMode(),
			FusionAlpha:    fusionAlpha(),
		},
		GraphOptions: store.GraphOptions{
			GraphGamma:      graphGamma(),
			GraphHopCap:     graphHopCap(),
			GraphLaneWeight: graphLaneWeight(),
		},
		RerankOptions: store.RerankOptions{
			RerankPool: rerankPool(),
		},
		InfraOptions: store.InfraOptions{
			DisableCoAccess: os.Getenv("DEX_COACCESS") == "0",
			VectorQuant:     vectorQuant(),
		},
	}
	// Assign through a typed-nil check: a (*rerank.Client)(nil) stored
	// in the Reranker interface field would still compare != nil, and
	// store.Search would dispatch into a nil receiver.
	if rc := newRerankClient(); rc != nil {
		opts.Reranker = rc
	}
	return opts
}

// vectorQuant reads DEX_VECTOR_QUANT — the chunk_vecs KNN encoding.
// "int8" selects scalar quantization (~4× smaller, faster integer cosine);
// anything else (incl. unset) keeps full-precision float32. Flipping it on
// an existing index rebuilds chunk_vecs from chunks.vec on the next Open.
func vectorQuant() string {
	raw := strings.TrimSpace(os.Getenv("DEX_VECTOR_QUANT"))
	switch strings.ToLower(raw) {
	case "", "none", "float32", "f32", "int8":
		return raw
	default:
		fmt.Fprintf(os.Stderr, "warning: DEX_VECTOR_QUANT=%q unrecognized; using float32\n", raw)
		return ""
	}
}

// fusionMode reads DEX_FUSION_MODE — score-fusion strategy for the dense+BM25 lanes.
// Default is FusionLinear (convex combination, α=fusionAlpha). Calibrated in #317:
// a leave-one-repo-out sweep over the multi-repo corpus picked FusionLinear α=0.7 in
// all 5 folds (+3.3% NDCG / +3pts Recall vs RRF, de-contaminated), and a dex-self
// confirmation showed +145% NDCG / +113% Recall over RRF. Set DEX_FUSION_MODE=rrf to
// fall back to rank-only Reciprocal Rank Fusion.
func fusionMode() store.FusionMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEX_FUSION_MODE"))) {
	case "", "linear":
		return store.FusionLinear
	case "rrf":
		return store.FusionRRF
	default:
		fmt.Fprintf(os.Stderr, "warning: DEX_FUSION_MODE=%q unrecognized; using linear\n", os.Getenv("DEX_FUSION_MODE"))
		return store.FusionLinear
	}
}

// fusionAlpha reads DEX_FUSION_ALPHA — dense weight for FusionLinear (0 < α ≤ 1).
// Default 0.7: the leave-one-repo-out corpus sweep in #317 found a clean interior
// optimum at α=0.7 (NDCG peaked there and fell off toward both pure-BM25 and
// pure-dense), confirmed on dex-self. Values outside (0,1] are rejected with a warning.
func fusionAlpha() float32 {
	raw := os.Getenv("DEX_FUSION_ALPHA")
	if raw == "" {
		return 0.7
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 || v > 1 {
		fmt.Fprintf(os.Stderr, "warning: DEX_FUSION_ALPHA=%q is not in (0,1]; using default (0.7)\n", raw)
		return 0.7
	}
	return float32(v)
}

// graphGamma reads DEX_GRAPH_GAMMA — the per-hop decay for the graph lane.
// Zero (unset/invalid) lets the store apply its default (defaultGraphGamma).
// Valid range is (0,1]; out-of-range values are ignored.
func graphGamma() float32 {
	raw := os.Getenv("DEX_GRAPH_GAMMA")
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 || v > 1 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_GAMMA=%q is not in (0,1]; using default\n", raw)
		return 0
	}
	return float32(v)
}

// graphHopCap reads DEX_GRAPH_HOP_CAP — the spreading-activation depth.
// Zero (unset/invalid) lets the store apply its default (defaultGraphHopCap).
func graphHopCap() int {
	raw := os.Getenv("DEX_GRAPH_HOP_CAP")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_HOP_CAP=%q is not a positive integer; using default\n", raw)
		return 0
	}
	return n
}

// graphLaneWeight reads DEX_GRAPH_WEIGHT — flat multiplier on the graph-proximity lane.
// Zero (unset/invalid) lets the store apply its default (defaultGraphLaneWeight = 1.0).
// Must be > 0; out-of-range values are ignored.
func graphLaneWeight() float32 {
	raw := os.Getenv("DEX_GRAPH_WEIGHT")
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil || v <= 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_GRAPH_WEIGHT=%q is not a positive number; using default\n", raw)
		return 0
	}
	return float32(v)
}

func parseDuration(envVar, raw string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a Go duration; using %s\n", envVar, raw, def)
		return def
	}
	return d
}

// maxHitsPerFile reads DEX_MAX_HITS_PER_FILE from the environment.
// Zero means no per-file cap (default). Positive values enforce result
// diversity — useful when a single heavily-matched file would otherwise
// dominate the top-k results.
func maxHitsPerFile() int {
	raw := os.Getenv("DEX_MAX_HITS_PER_FILE")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fmt.Fprintf(os.Stderr, "warning: DEX_MAX_HITS_PER_FILE=%q is not a non-negative integer; ignoring\n", raw)
		return 0
	}
	return n
}

func openStore(ctx context.Context, dbPath string) (*store.Store, error) {
	return store.OpenWith(ctx, dbPath, storeOpts())
}

// cliLogger returns a stderr text logger. Used for the CLI commands
// (index/watch) so verbose output goes to stderr without polluting
// stdout (which the MCP server uses for JSON-RPC).
func cliLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// indexProgressPrinter returns an index.Options.Progress callback that writes
// human-readable progress to stderr (text mode) or NDJSON to stdout (json mode).
// Returns nil when no TTY is attached and format is text — avoids cluttering
// piped output with carriage-return lines.
func indexProgressPrinter(format string) func(phase string, done, total int) {
	if format == "json" {
		return func(phase string, done, total int) {
			pct := 0.0
			if total > 0 {
				pct = float64(done) / float64(total)
			}
			_ = json.NewEncoder(os.Stdout).Encode(struct {
				Type     string  `json:"type"`
				Phase    string  `json:"phase"`
				Done     int     `json:"done"`
				Total    int     `json:"total"`
				Progress float64 `json:"progress"`
			}{"progress", phase, done, total, pct})
		}
	}
	// Text mode: only emit when stderr is a terminal.
	fi, err := os.Stderr.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	var lastPhase string
	return func(phase string, done, total int) {
		if phase != lastPhase {
			if lastPhase != "" {
				fmt.Fprint(os.Stderr, "\r\033[K") // clear the in-progress line
			}
			lastPhase = phase
		}
		switch phase {
		case "walk":
			fmt.Fprintf(os.Stderr, "\r  walk: %d files scanned", done)
		case "embed":
			if total > 0 {
				pct := done * 100 / total
				fmt.Fprintf(os.Stderr, "\r  embedding: %d/%d chunks (%d%%)", done, total, pct)
			} else {
				fmt.Fprintf(os.Stderr, "\r  embedding: %d chunks", done)
			}
			if done >= total && total > 0 {
				fmt.Fprint(os.Stderr, "\r\033[K") // clear when done
			}
		}
	}
}

// warnIfNoInclude prints a prominent notice when a project has no
// `index.include` in .dex/config.yml. Indexing is opt-in, so without
// it the run produces an empty index — surface that instead of letting
// it look like a silent success.
func warnIfNoInclude(ig *ignore.Matcher, root string) {
	if !ig.IncludeConfigured() {
		fmt.Fprintf(os.Stderr,
			"⚠ no index.include in %s/.dex/config.yml — nothing will be indexed\n", root)
	}
}

// isStaleEmbed reports whether the index's recorded embed model is known to
// differ from the active model. It only compares against the explicit
// DEX_EMBED_MODEL env var — no network calls. Returns false when either side
// is unknown (empty), so pre-migration indexes are never falsely flagged.
func isStaleEmbed(indexModel string) bool {
	active := os.Getenv("DEX_EMBED_MODEL")
	return active != "" && indexModel != "" && active != indexModel
}

// newEmbedClient constructs an embed.Embedder from env vars, falling back to
// indexModel (the model recorded in the target index) when DEX_EMBED_MODEL is
// unset. Callers that have an open *store.Store should pass st.EmbedModel();
// callers that are building a fresh index (or have no store context) pass "".
// If DEX_EMBED_DIM is set, the returned embedder truncates vectors to that
// many dimensions and re-normalises (Matryoshka truncation).
