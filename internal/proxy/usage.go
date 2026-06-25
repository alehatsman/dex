package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// ProviderUsage holds normalized token counts from one streaming response.
type ProviderUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// scanSSEChunk scans one SSE "data: ..." payload for provider usage fields.
// Uses bytes.Contains as a fast pre-filter; only unmarshals on a hit.
// Returns zero value when no usage found.
func scanSSEChunk(data []byte) ProviderUsage {
	// Strip SSE "data: " prefix.
	d := data
	if after, ok := bytes.CutPrefix(d, []byte("data: ")); ok {
		d = after
	}
	d = bytes.TrimSpace(d)

	// Skip SSE keep-alive and non-JSON lines.
	if len(d) == 0 || d[0] != '{' {
		return ProviderUsage{}
	}

	// Fast pre-filter: only pay JSON decode cost when a usage key is present.
	hasUsage := bytes.Contains(d, []byte(`"usage"`))
	hasUsageMeta := bytes.Contains(d, []byte("usageMetadata"))
	if !hasUsage && !hasUsageMeta {
		return ProviderUsage{}
	}

	// Try Anthropic format first — it has a "type" field.
	if hasUsage {
		// message_start: input tokens + cache fields.
		var start struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(d, &start); err == nil && start.Type == "message_start" {
			return ProviderUsage{
				InputTokens:      start.Message.Usage.InputTokens,
				CacheWriteTokens: start.Message.Usage.CacheCreationInputTokens,
				CacheReadTokens:  start.Message.Usage.CacheReadInputTokens,
			}
		}

		// message_delta: output tokens.
		var delta struct {
			Type  string `json:"type"`
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(d, &delta); err == nil && delta.Type == "message_delta" {
			return ProviderUsage{
				OutputTokens: delta.Usage.OutputTokens,
			}
		}

		// OpenAI format: has "choices" array and usage at top level.
		var oai struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				PromptDetails    struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(d, &oai); err == nil && oai.Choices != nil {
			return ProviderUsage{
				InputTokens:     oai.Usage.PromptTokens,
				OutputTokens:    oai.Usage.CompletionTokens,
				CacheReadTokens: oai.Usage.PromptDetails.CachedTokens,
			}
		}
	}

	// Gemini format: usageMetadata at top level.
	if hasUsageMeta {
		var gemini struct {
			UsageMetadata struct {
				PromptTokenCount     int64 `json:"promptTokenCount"`
				CandidatesTokenCount int64 `json:"candidatesTokenCount"`
				CachedContentTokens int64 `json:"cachedContentTokenCount"`
			} `json:"usageMetadata"`
		}
		if err := json.Unmarshal(d, &gemini); err == nil {
			return ProviderUsage{
				InputTokens:     gemini.UsageMetadata.PromptTokenCount,
				OutputTokens:    gemini.UsageMetadata.CandidatesTokenCount,
				CacheReadTokens: gemini.UsageMetadata.CachedContentTokens,
			}
		}
	}

	return ProviderUsage{}
}

// usageTeeWriter wraps http.ResponseWriter and intercepts each Write to
// scan SSE chunks for usage tokens. When the response ends, it calls
// notify with the accumulated totals.
type usageTeeWriter struct {
	http.ResponseWriter
	buf    []byte // carry-over from incomplete SSE lines
	usage  ProviderUsage
	notify func(ProviderUsage)
}

func newUsageTeeWriter(w http.ResponseWriter, notify func(ProviderUsage)) *usageTeeWriter {
	return &usageTeeWriter{
		ResponseWriter: w,
		notify:         notify,
	}
}

// Write intercepts each chunk, scans for usage, passes through to the real writer.
func (u *usageTeeWriter) Write(p []byte) (int, error) {
	// Build a combined view for scanning: carry-over + new data.
	var scan []byte
	if len(u.buf) > 0 {
		scan = append(u.buf, p...)
		u.buf = nil
	} else {
		scan = p
	}

	// Process only newline-terminated lines. A trailing partial line (no final \n)
	// is carried over to the next Write call. This is the key correctness property:
	// we never treat a truncated SSE line as complete.
	for {
		idx := bytes.IndexByte(scan, '\n')
		if idx < 0 {
			// No newline — entire remainder is partial; carry it over.
			u.buf = append(u.buf[:0], scan...)
			break
		}
		line := scan[:idx] // excludes the '\n'
		if bytes.HasPrefix(line, []byte("data: ")) {
			u.addUsage(scanSSEChunk(line))
		}
		scan = scan[idx+1:]
	}

	// Always forward the original bytes unchanged.
	return u.ResponseWriter.Write(p)
}

// addUsage accumulates non-zero fields from u into the running total.
func (u *usageTeeWriter) addUsage(v ProviderUsage) {
	if v.InputTokens != 0 {
		u.usage.InputTokens += v.InputTokens
	}
	if v.OutputTokens != 0 {
		u.usage.OutputTokens += v.OutputTokens
	}
	if v.CacheReadTokens != 0 {
		u.usage.CacheReadTokens += v.CacheReadTokens
	}
	if v.CacheWriteTokens != 0 {
		u.usage.CacheWriteTokens += v.CacheWriteTokens
	}
	if v.ReasoningTokens != 0 {
		u.usage.ReasoningTokens += v.ReasoningTokens
	}
}

// Flush implements http.Flusher if the underlying writer supports it.
func (u *usageTeeWriter) Flush() {
	if f, ok := u.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Done should be called after the response completes to fire notify.
func (u *usageTeeWriter) Done() { u.notify(u.usage) }
