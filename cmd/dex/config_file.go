// .dex/config.toml operational config — collapses the DEX_* env-var sprawl
// (MCP env block + systemd unit + shell) into one per-project file.
//
// Precedence is strict: env var > config file > built-in default. We only
// ever fill gaps — a DEX_* var already present in the environment is never
// overwritten — so the dotfiles/systemd env layer keeps winning and nothing
// that worked before changes.
//
// The file extends the same .dex/config.toml that carries [index] (see
// internal/ignore/config.go). Recognized scalar sections:
//
//	[endpoints]
//	embed = "http://localhost:11434"   # -> DEX_EMBED_URL
//	chat  = "http://localhost:11434"   # -> DEX_CHAT_URL
//
//	[models]
//	embed = "mxbai-embed-large"        # -> DEX_EMBED_MODEL
//	chat  = "qwen2.5-coder:14b"        # -> DEX_CHAT_MODEL
//
//	[tools]
//	tier = "power"                     # -> DEX_TOOLS
//
//	[env]                              # escape hatch: any DEX_* knob verbatim
//	DEX_EMBED_CONCURRENCY = 8
//
// DEX_SERVE_TOKEN is a secret and is deliberately NOT readable from the file.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configKeyMap maps "section.key" in .dex/config.toml to the DEX_* env var it
// populates. Keep in sync with the allEnvVars table in env.go.
var configKeyMap = map[string]string{
	// [endpoints]
	"endpoints.embed":    "DEX_EMBED_URL",
	"endpoints.chat":     "DEX_CHAT_URL",
	"endpoints.rerank":   "DEX_RERANK_URL",
	"endpoints.summary":  "DEX_SUMMARY_URL",
	"endpoints.compress": "DEX_COMPRESS_URL",
	"endpoints.draft":    "DEX_DRAFT_URL",
	// [models]
	"models.embed":    "DEX_EMBED_MODEL",
	"models.chat":     "DEX_CHAT_MODEL",
	"models.rerank":   "DEX_RERANK_MODEL",
	"models.summary":  "DEX_SUMMARY_MODEL",
	"models.chunk":    "DEX_CHUNK_SUMMARY_MODEL",
	"models.file":     "DEX_FILE_SUMMARY_MODEL",
	"models.package":  "DEX_PACKAGE_SUMMARY_MODEL",
	"models.repo":     "DEX_REPO_SUMMARY_MODEL",
	"models.compress": "DEX_COMPRESS_MODEL",
	"models.draft":    "DEX_DRAFT_MODEL",
	// [tools]
	"tools.tier":              "DEX_TOOLS",
	"tools.auto_summarize":    "DEX_AUTO_SUMMARIZE",
	"tools.disable_rerank":    "DEX_DISABLE_RERANK",
	"tools.disable_bm25":      "DEX_DISABLE_BM25",
	"tools.rerank_style":      "DEX_RERANK_STYLE",
	"tools.embed_batch":       "DEX_EMBED_BATCH",
	"tools.max_hits_per_file": "DEX_MAX_HITS_PER_FILE",
}

// secretVars are never populated from the config file — they must come from
// the environment so tokens don't get checked into a repo.
var secretVars = map[string]bool{"DEX_SERVE_TOKEN": true}

// fileSourcedKeys records which DEX_* vars were populated from .dex/config.toml
// rather than the real environment. `dex env` reads it to label the source.
var fileSourcedKeys = map[string]bool{}

// parseConfigToml reads .dex/config.toml at root and returns a map of DEX_* var
// name -> value for every recognized setting. A missing file yields an empty
// map and no error. It parses a tiny scalar TOML subset ([sections] and
// `key = scalar` lines); array values (the [index] lists) are skipped here —
// internal/ignore owns those. No TOML dependency.
func parseConfigToml(root string) (map[string]string, error) {
	out := map[string]string{}
	path := filepath.Join(root, ".dex", "config.toml")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	defer func() { _ = f.Close() }()

	section := ""
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(stripConfigComment(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		rawKey, rawVal, ok := strings.Cut(line, "=")
		if !ok {
			return out, fmt.Errorf("%s:%d: expected key = value", path, ln)
		}
		key := strings.TrimSpace(rawKey)
		val := strings.TrimSpace(rawVal)
		if strings.HasPrefix(val, "[") {
			continue // array value — owned by internal/ignore ([index] lists)
		}
		val = strings.Trim(val, `"`)

		var envName string
		switch section {
		case "env":
			// Escape hatch: keys are DEX_* names verbatim.
			if strings.HasPrefix(key, "DEX_") {
				envName = key
			}
		default:
			envName = configKeyMap[section+"."+key]
		}
		if envName == "" || secretVars[envName] {
			continue
		}
		out[envName] = val
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// applyProjectConfig loads .dex/config.toml from root and sets each recognized
// DEX_* var that is NOT already present in the environment (env wins). Keys it
// sets are recorded in fileSourcedKeys for `dex env` source labeling.
func applyProjectConfig(root string) error {
	vals, err := parseConfigToml(root)
	if err != nil {
		return err
	}
	for name, val := range vals {
		if _, set := os.LookupEnv(name); set {
			continue // env var present — it wins
		}
		if err := os.Setenv(name, val); err != nil {
			return err
		}
		fileSourcedKeys[name] = true
	}
	return nil
}

// stripConfigComment removes a trailing '#' comment, ignoring '#' inside
// double quotes.
func stripConfigComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}
