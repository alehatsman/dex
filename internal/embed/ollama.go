package embed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"qwen3-embedding",
	"mxbai-embed-large",
	"nomic-embed-text",
	"granite-embedding",
	"bge-m3",
	"snowflake-arctic-embed",
	"bge-large",
	"bge-base",
	"all-minilm",
}

// preferredChatModels lists known ollama chat/LLM model name substrings in
// priority order, skewed toward code understanding.
var preferredChatModels = []string{
	"qwen2.5-coder",
	"deepseek-coder-v2",
	"deepseek-coder",
	"starcoder2",
	"codegemma",
	"codellama",
	"qwen2.5",
	"llama3.2",
	"llama3.1",
	"llama3",
	"mistral",
	"phi4",
	"phi3",
	"gemma2",
	"gemma",
}

// OllamaModel is the result of a successful ollama embedding model discovery.
type OllamaModel struct {
	Name string // e.g. "nomic-embed-text:latest"
	URL  string // e.g. "http://localhost:11434"
}

// OllamaScan is the full result of an ollama discovery probe.
// URL is set whenever ollama is reachable; EmbedModels/ChatModels may be
// empty if no recognised models of that kind are installed.
type OllamaScan struct {
	URL         string   // e.g. "http://localhost:11434"
	EmbedModels []string // recognised embed models, highest-priority first
	ChatModels  []string // recognised chat/LLM models, highest-priority first
}

// DetectOllama probes the local ollama daemon (localhost:11434) for a known
// embedding model. Returns the highest-priority match and true if found.
// Uses a 2-second timeout so CLI startup is not delayed when ollama is absent.
func DetectOllama(ctx context.Context) (OllamaModel, bool) {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return detectOllamaFrom(tctx, ollamaBase)
}

// DetectOllamaChat probes the local ollama daemon for a chat/LLM model,
// preferring code-capable models. Uses a 2-second timeout.
func DetectOllamaChat(ctx context.Context) (OllamaModel, bool) {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	scan, ok := scanOllamaFrom(tctx, ollamaBase)
	if !ok || len(scan.ChatModels) == 0 {
		return OllamaModel{}, false
	}
	return OllamaModel{Name: scan.ChatModels[0], URL: scan.URL}, true
}

// ScanOllama probes the local ollama daemon and returns all recognised
// embedding and chat models ordered by priority. Returns (scan, true)
// whenever ollama is reachable. Uses a 2-second timeout.
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
	// Verify each candidate is actually live — a model listed in /api/tags may
	// not be loadable (removed from disk, corrupt layer, etc.). Use a real embed
	// probe here (not Health) because /v1/models is server-level and wouldn't
	// distinguish a broken model from a working one.
	for _, name := range scan.EmbedModels {
		c := New(base, name, 1, 2*time.Second)
		if _, err := c.embedBatch(ctx, []string{"ping"}); err == nil {
			return OllamaModel{Name: name, URL: base}, true
		}
	}
	return OllamaModel{}, false
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
	var embedFound, chatFound []ranked
	for _, m := range body.Models {
		if pri := embedModelPriority(m.Name); pri >= 0 {
			embedFound = append(embedFound, ranked{m.Name, pri})
			continue // embed-only models skip the chat list
		}
		if pri := chatModelPriority(m.Name); pri >= 0 {
			chatFound = append(chatFound, ranked{m.Name, pri})
		}
	}
	sort.Slice(embedFound, func(i, j int) bool { return embedFound[i].pri > embedFound[j].pri })
	sort.Slice(chatFound, func(i, j int) bool { return chatFound[i].pri > chatFound[j].pri })

	embedNames := make([]string, len(embedFound))
	for i, f := range embedFound {
		embedNames[i] = f.name
	}
	chatNames := make([]string, len(chatFound))
	for i, f := range chatFound {
		chatNames[i] = f.name
	}
	return OllamaScan{URL: base, EmbedModels: embedNames, ChatModels: chatNames}, true
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

// chatModelPriority returns a priority score for a chat/LLM model name.
// Returns -1 for models not in the known chat list.
func chatModelPriority(name string) int {
	lower := strings.ToLower(name)
	for i, pattern := range preferredChatModels {
		if strings.Contains(lower, pattern) {
			return len(preferredChatModels) - i
		}
	}
	return -1
}

// DefaultPullModel is the ollama model pulled by PullOllamaModel when no
// specific model is requested.
const DefaultPullModel = "qwen3-embedding:4b"

// PullOllamaModel asks the local ollama daemon to pull model (e.g.
// "nomic-embed-text"). Progress lines from the streaming pull response are
// written to progress (may be nil). Returns when the pull is complete or the
// context is cancelled.
func PullOllamaModel(ctx context.Context, model string, progress io.Writer) error {
	payload, err := json.Marshal(map[string]any{"name": model, "stream": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaBase+"/api/pull", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama pull: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ollama pull: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	// Stream NDJSON progress lines.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg struct {
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Error != "" {
			return fmt.Errorf("ollama pull: %s", msg.Error)
		}
		if progress != nil && msg.Status != "" {
			_, _ = fmt.Fprintln(progress, msg.Status)
		}
	}
	return scanner.Err()
}
