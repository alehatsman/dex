package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

const ollamaBase = "http://localhost:11434"

// preferredEmbedModels lists known ollama embedding model name substrings in
// priority order (index 0 = highest priority). Matching is case-insensitive
// substring so "nomic-embed-text:latest" matches "nomic-embed-text".
var preferredEmbedModels = []string{
	"mxbai-embed-large",
	"nomic-embed-text",
	"granite-embedding",
	"bge-m3",
	"snowflake-arctic-embed",
	"bge-large",
	"bge-base",
	"all-minilm",
}

// OllamaModel is the result of a successful ollama embedding model discovery.
type OllamaModel struct {
	Name string // e.g. "nomic-embed-text:latest"
	URL  string // e.g. "http://localhost:11434"
}

// OllamaScan is the full result of an ollama discovery probe.
// URL is set whenever ollama is reachable; EmbedModels may be empty if no
// recognised embedding models are installed.
type OllamaScan struct {
	URL         string   // e.g. "http://localhost:11434"
	EmbedModels []string // recognised embed models, highest-priority first
}

// DetectOllama probes the local ollama daemon (localhost:11434) for a known
// embedding model. Returns the highest-priority match and true if found.
// Uses a 2-second timeout so CLI startup is not delayed when ollama is absent.
func DetectOllama(ctx context.Context) (OllamaModel, bool) {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return detectOllamaFrom(tctx, ollamaBase)
}

// ScanOllama probes the local ollama daemon and returns all recognised
// embedding models ordered by priority. Returns (scan, true) whenever ollama
// is reachable — EmbedModels may still be empty if no known embed models are
// installed. Uses a 2-second timeout.
func ScanOllama(ctx context.Context) (OllamaScan, bool) {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return scanOllamaFrom(tctx, ollamaBase)
}

func detectOllamaFrom(ctx context.Context, base string) (OllamaModel, bool) {
	scan, ok := scanOllamaFrom(ctx, base)
	if !ok || len(scan.EmbedModels) == 0 {
		return OllamaModel{}, false
	}
	return OllamaModel{Name: scan.EmbedModels[0], URL: scan.URL}, true
}

func scanOllamaFrom(ctx context.Context, base string) (OllamaScan, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return OllamaScan{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return OllamaScan{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OllamaScan{}, false
	}

	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return OllamaScan{}, false
	}

	type ranked struct {
		name string
		pri  int
	}
	var found []ranked
	for _, m := range body.Models {
		if pri := embedModelPriority(m.Name); pri >= 0 {
			found = append(found, ranked{m.Name, pri})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].pri > found[j].pri })

	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f.name
	}
	return OllamaScan{URL: base, EmbedModels: names}, true
}

// embedModelPriority returns a priority score for a model name (higher = better).
// Returns -1 for models not in the known embedding list.
func embedModelPriority(name string) int {
	lower := strings.ToLower(name)
	for i, pattern := range preferredEmbedModels {
		if strings.Contains(lower, pattern) {
			return len(preferredEmbedModels) - i
		}
	}
	return -1
}
