package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
	"github.com/alehatsman/dex/internal/rerank"
	"github.com/alehatsman/dex/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── tool: index_status ───────────────────────────────────────────────────

type StatusInput struct{}

// BreakerStatus mirrors rerank.BreakerState in the index_status JSON.
// Surfaced under StatusOutput.RerankBreaker so operators can see when
// the breaker is open (and until when) without grepping logs.
type BreakerStatus struct {
	Open             bool   `json:"open"`
	OpenUntil        string `json:"open_until,omitempty"`
	ConsecutiveFails int    `json:"consecutive_fails,omitempty"`
}

type ProjectStatus struct {
	ID               string `json:"id"`
	Root             string `json:"root,omitempty"`
	Chunks           int    `json:"chunks"`
	Files            int    `json:"files"`
	Dim              int    `json:"dim"`
	EmbedModel       string `json:"embed_model,omitempty"`
	LastIndexed      string `json:"last_indexed,omitempty"`
	Indexing         bool   `json:"indexing,omitempty"`           // a re-index is underway; counts are mid-rebuild (#531)
	GitRecencyActive bool   `json:"git_recency_active,omitempty"` // git recency/dirty boost is wired for this project
}

type StatusOutput struct {
	Endpoint           string          `json:"endpoint"`
	Reachable          bool            `json:"reachable"`
	Model              string          `json:"model"`
	ChatEndpoint       string          `json:"chat_endpoint,omitempty"`
	ChatReachable      bool            `json:"chat_reachable,omitempty"`
	ChatModelAvailable bool            `json:"chat_model_available,omitempty"`
	ChatModel          string          `json:"chat_model,omitempty"`
	RerankEndpoint     string          `json:"rerank_endpoint,omitempty"`
	RerankReachable    bool            `json:"rerank_reachable,omitempty"`
	RerankModel        string          `json:"rerank_model,omitempty"`
	RerankBreaker      *BreakerStatus  `json:"rerank_breaker,omitempty"`
	OllamaEndpoint     string          `json:"ollama_endpoint,omitempty"`
	OllamaEmbedModels  []string        `json:"ollama_embed_models,omitempty"`
	OllamaChatModels   []string        `json:"ollama_chat_models,omitempty"`
	Version            string          `json:"version"`
	IndexDir           string          `json:"index_dir"`
	Projects           []ProjectStatus `json:"projects,omitempty"`
	Error              string          `json:"error,omitempty"`
}

// healthChecker abstracts a client that can report reachability.
type healthChecker interface {
	Health(ctx context.Context) error
}

func (s *Server) status(ctx context.Context, _ *sdk.CallToolRequest, _ StatusInput) (*sdk.CallToolResult, StatusOutput, error) {
	out := StatusOutput{
		Version:  Version,
		IndexDir: s.IndexDir,
	}
	if s.EmbedClient != nil {
		out.Endpoint = s.EmbedClient.Endpoint()
		out.Model = s.EmbedClient.ModelName()
	} else {
		// Lean profile (DEX_EMBED_ENGINE=none): no embedder wired.
		out.Model = "none (lean profile)"
	}

	// Populate optional endpoint metadata before probing (read-only).
	if s.ChatClient != nil {
		out.ChatEndpoint = s.ChatClient.Endpoint()
		out.ChatModel = s.ChatClient.ModelName()
	}
	if s.RerankClient != nil {
		out.RerankEndpoint = s.RerankClient.Endpoint()
		out.RerankModel = s.RerankClient.ModelName()
		// Surface circuit-breaker state if the rerank client is wrapped.
		// A type assertion avoids leaking rerank.Breaker into the server's
		// signature; callers that wire a bare client (no breaker) skip this.
		if br, ok := s.RerankClient.(interface{ State() rerank.BreakerState }); ok {
			st := br.State()
			bs := &BreakerStatus{Open: st.Open, ConsecutiveFails: st.ConsecutiveFails}
			if !st.OpenUntil.IsZero() {
				bs.OpenUntil = st.OpenUntil.Format(time.RFC3339)
			}
			out.RerankBreaker = bs
		}
	}

	// Probe all clients concurrently — each has a 3 s timeout; running
	// them in parallel keeps the total wall-clock cost at ~3 s instead
	// of up to 15 s when clients are unreachable.
	probe := func(wg *sync.WaitGroup, client healthChecker, setResult func(bool, string)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := client.Health(pctx); err != nil {
				setResult(false, err.Error())
			} else {
				setResult(true, "")
			}
		}()
	}

	var wg sync.WaitGroup
	if s.EmbedClient != nil {
		probe(&wg, s.EmbedClient, func(ok bool, errMsg string) {
			out.Reachable = ok
			out.Error = errMsg
		})
	}
	if s.ChatClient != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			err := s.ChatClient.Health(pctx)
			switch {
			case err == nil:
				out.ChatReachable = true
				out.ChatModelAvailable = true
			case errors.Is(err, chat.ErrModelNotFound):
				out.ChatReachable = true
				out.ChatModelAvailable = false
			default:
				out.ChatReachable = false
				out.ChatModelAvailable = false
			}
		}()
	}
	if s.RerankClient != nil {
		probe(&wg, s.RerankClient, func(ok bool, _ string) { out.RerankReachable = ok })
	}
	wg.Go(func() {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if scan, ok := embed.ScanOllama(pctx); ok {
			out.OllamaEndpoint = scan.URL
			out.OllamaEmbedModels = scan.EmbedModels
			out.OllamaChatModels = scan.ChatModels
		}
	})
	wg.Wait()

	if entries, err := os.ReadDir(s.IndexDir); err == nil {
		type result struct {
			ps ProjectStatus
			ok bool
		}
		results := make([]result, len(entries))
		sem := make(chan struct{}, 8)
		var pwg sync.WaitGroup
		for i, e := range entries {
			if !e.IsDir() {
				continue
			}
			dbPath := filepath.Join(s.IndexDir, e.Name(), "index.db")
			if _, err := os.Stat(dbPath); err != nil {
				continue
			}
			pwg.Add(1)
			go func(idx int, id, path string) {
				defer pwg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// Open an ephemeral handle, not the shared query cache:
				// index_status scans every project on disk (including ones
				// never queried), and closing a cached store would poison the
				// handle reused by the watcher and all other handlers, leaving
				// them with "sql: database is closed" until restart (#514).
				st, err := store.OpenWith(ctx, path, s.StoreOpts)
				if err != nil {
					return
				}
				stats, _ := st.Stats(ctx)
				root, _ := st.ProjectRoot(ctx)
				indexing, _ := st.IndexingInProgress(ctx)
				st.Close()
				gitActive := false
				if root != "" {
					_, gitErr := os.Stat(filepath.Join(root, ".git"))
					gitActive = gitErr == nil
				}
				ps := ProjectStatus{
					ID:               id,
					Root:             root,
					Chunks:           stats.Chunks,
					Files:            stats.Files,
					Dim:              stats.Dim,
					EmbedModel:       stats.EmbedModel,
					Indexing:         indexing,
					GitRecencyActive: gitActive,
				}
				if !stats.LastIndex.IsZero() {
					ps.LastIndexed = stats.LastIndex.Format(time.RFC3339)
				}
				results[idx] = result{ps: ps, ok: true}
			}(i, e.Name(), dbPath)
		}
		pwg.Wait()
		for _, r := range results {
			if r.ok {
				out.Projects = append(out.Projects, r.ps)
			}
		}
	}
	return nil, out, nil
}

// RunStdio starts the MCP server bound to stdin/stdout. Sets runCtx
// so per-project Watcher goroutines spawned during the session share
// this ctx and exit cleanly when it ends. Blocks until ctx is
// cancelled or the transport closes, then waits for any spawned
// watchers to drain.
