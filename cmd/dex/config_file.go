// .dex/config.yml operational config — collapses the DEX_* env-var sprawl
// (MCP env block + systemd unit + shell) into one per-project file.
//
// Precedence is strict: env var > config file > built-in default. We only
// ever fill gaps — a DEX_* var already present in the environment is never
// overwritten — so the dotfiles/systemd env layer keeps winning and nothing
// that worked before changes.
//
//	endpoints:
//	  embed: http://localhost:11434   # -> DEX_EMBED_URL
//	  chat:  http://localhost:11434   # -> DEX_CHAT_URL
//	models:
//	  embed: mxbai-embed-large        # -> DEX_EMBED_MODEL
//	  chat:  qwen2.5-coder:14b        # -> DEX_CHAT_MODEL
//	tools:
//	  tier: power                     # -> DEX_TOOLS
//	env:                              # escape hatch: any DEX_* knob verbatim
//	  DEX_EMBED_CONCURRENCY: 8
//
// DEX_SERVE_TOKEN is a secret and is deliberately NOT readable from the file.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// fileConfig is the parsed .dex/config.yml. Values are read as strings where
// possible; tools/env accept scalars (string|bool|int) coerced via scalarStr.
type fileConfig struct {
	Endpoints map[string]string `yaml:"endpoints"`
	Models    map[string]string `yaml:"models"`
	Tools     map[string]any    `yaml:"tools"`
	Env       map[string]any    `yaml:"env"`
}

// sectionKeyMap maps a "section.key" to the DEX_* env var it populates. Keep
// in sync with the allEnvVars table in env.go.
var sectionKeyMap = map[string]string{
	"endpoints.embed":         "DEX_EMBED_URL",
	"endpoints.chat":          "DEX_CHAT_URL",
	"endpoints.rerank":        "DEX_RERANK_URL",
	"endpoints.summary":       "DEX_SUMMARY_URL",
	"endpoints.compress":      "DEX_COMPRESS_URL",
	"endpoints.draft":         "DEX_DRAFT_URL",
	"models.embed":            "DEX_EMBED_MODEL",
	"models.chat":             "DEX_CHAT_MODEL",
	"models.rerank":           "DEX_RERANK_MODEL",
	"models.summary":          "DEX_SUMMARY_MODEL",
	"models.chunk":            "DEX_CHUNK_SUMMARY_MODEL",
	"models.file":             "DEX_FILE_SUMMARY_MODEL",
	"models.package":          "DEX_PACKAGE_SUMMARY_MODEL",
	"models.repo":             "DEX_REPO_SUMMARY_MODEL",
	"models.compress":         "DEX_COMPRESS_MODEL",
	"models.draft":            "DEX_DRAFT_MODEL",
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

// fileSourcedKeys records which DEX_* vars were populated from .dex/config.yml
// rather than the real environment. `dex env` reads it to label the source.
var fileSourcedKeys = map[string]bool{}

// parseConfigFile reads .dex/config.yml at root and returns a map of DEX_* var
// name -> value for every recognized setting. A missing file yields an empty
// map and no error.
func parseConfigFile(root string) (map[string]string, error) {
	out := map[string]string{}
	path := filepath.Join(root, ".dex", "config.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}

	var cfg fileConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return out, fmt.Errorf("%s: %w", path, err)
	}

	put := func(section string, m map[string]string) {
		for k, v := range m {
			if name := sectionKeyMap[section+"."+k]; name != "" && !secretVars[name] {
				out[name] = v
			}
		}
	}
	putAny := func(section string, m map[string]any) {
		for k, v := range m {
			if name := sectionKeyMap[section+"."+k]; name != "" && !secretVars[name] {
				out[name] = scalarStr(v)
			}
		}
	}

	put("endpoints", cfg.Endpoints)
	put("models", cfg.Models)
	putAny("tools", cfg.Tools)

	// env: escape hatch — keys are DEX_* names verbatim.
	for k, v := range cfg.Env {
		if len(k) > 4 && k[:4] == "DEX_" && !secretVars[k] {
			out[k] = scalarStr(v)
		}
	}
	return out, nil
}

// scalarStr renders a YAML scalar (string|bool|int|float) as the string a
// DEX_* reader expects.
func scalarStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// applyProjectConfig loads .dex/config.yml from root and sets each recognized
// DEX_* var that is NOT already present in the environment (env wins). Keys it
// sets are recorded in fileSourcedKeys for `dex env` source labeling.
func applyProjectConfig(root string) error {
	vals, err := parseConfigFile(root)
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
