package embed

import (
	"context"
	"encoding/json"
	"net/http"
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

// DetectOllama probes the local ollama daemon (localhost:11434) for a known
// embedding model. Returns the highest-priority match and true if found.
// Uses a 2-second timeout so CLI startup is not delayed when ollama is absent.
func DetectOllama(ctx context.Context) (OllamaModel, bool) {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return detectOllamaFrom(tctx, ollamaBase)
}

func detectOllamaFrom(ctx context.Context, base string) (OllamaModel, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return OllamaModel{}, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return OllamaModel{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OllamaModel{}, false
	}

	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return OllamaModel{}, false
	}

	best, bestPri := "", -1
	for _, m := range body.Models {
		if pri := embedModelPriority(m.Name); pri > bestPri {
			best, bestPri = m.Name, pri
		}
	}
	if best == "" {
		return OllamaModel{}, false
	}
	return OllamaModel{Name: best, URL: base}, true
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
