package retrieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/alehatsman/dex/internal/chat"
)

// Answer synthesis for the `ask` tool. SynthesizeAnswer turns the
// evidence bundle the transport assembles (suggested_reads with inlined
// content, symbol signatures+docs, graph edges) into a short grounded
// prose answer via the chat leg.
//
// Synthesis is best-effort and never blocks a result: a nil/unreachable
// chat client returns an empty answer and no error, so the caller ships
// the evidence bundle unchanged. A non-unreachable chat error is
// returned so the caller can note it in a hint.

// answerTruncatedMarker is appended to a synthesized answer that hit the
// token ceiling (finish_reason=length), so a mid-sentence cut is never
// mistaken for a complete answer (#568).
const answerTruncatedMarker = " […answer truncated at token limit]"

// answerMaxTokensFor caps synthesis length. Answers are meant to be a
// tight paragraph or two with citations, not an essay — bounding tokens
// also bounds generation time on the (shared) GPU. Exploration intents
// (architecture, package_topology) get a higher ceiling: the caller is
// forming a mental model and their evidence pool is wider (see
// InlineCapsFor), so the flat 400-token cap truncated those answers
// mid-sentence (#568). Targeted intents stay at 400.
func answerMaxTokensFor(intent string) int {
	switch intent {
	case IntentArchitecture, IntentPackageTopology:
		return 900
	default:
		return 400
	}
}

// SynthesizeAnswer produces a grounded prose answer from pre-assembled
// evidence text. It returns ("", "", nil) when synthesis is disabled
// (nil client), there is no evidence, the model is unreachable, or the
// model returns nothing — all degrade silently to the evidence-only
// bundle. A non-unreachable chat error is returned as hintErr.
//
// When logTok is non-nil, tokens are streamed to it as they arrive, so
// the transport can forward partial output before the call completes.
// cache must be non-nil; identical (question,intent,model,evidence)
// repeats hit it instead of the GPU.
func SynthesizeAnswer(ctx context.Context, client chat.Chatter, cache *AnswerCache, intent, question, evidence string, logTok func(string)) (answer, model string, hintErr error) {
	if client == nil || strings.TrimSpace(evidence) == "" {
		return "", "", nil
	}

	model = client.ModelName()
	key := cache.key(question, intent, model, evidence)
	if cached, ok := cache.get(key); ok {
		return cached, model, nil
	}

	msgs := []chat.Message{
		{Role: "system", Content: answerSystemPrompt},
		{Role: "user", Content: buildAnswerUser(question, intent, evidence)},
	}
	opts := chat.Options{MaxTokens: answerMaxTokensFor(intent)}

	var (
		resp chat.Response
		err  error
	)
	if logTok != nil {
		resp, err = client.GenerateStream(ctx, msgs, opts, logTok)
	} else {
		resp, err = client.Generate(ctx, msgs, opts)
	}
	if err != nil {
		// Unreachable → silent degrade. Any other chat error is surfaced
		// so the caller can attach a hint; the answer still degrades to
		// the evidence-only bundle.
		if errors.Is(err, chat.ErrUnreachable) {
			return "", "", nil
		}
		return "", "", err
	}
	ans := strings.TrimSpace(resp.Content)
	if ans == "" {
		return "", "", nil
	}
	// A "length" finish means the model hit the token ceiling mid-answer:
	// the prose just stops, often mid-sentence. Mark it so the caller never
	// mistakes a truncated lead for a complete one (#568). When streaming,
	// emit the marker through logTok too so the forwarded output matches.
	if resp.FinishReason == "length" {
		ans += answerTruncatedMarker
		if logTok != nil {
			logTok(answerTruncatedMarker)
		}
	}
	cache.put(key, ans)
	return ans, model, nil
}

const answerSystemPrompt = "You are a code-intelligence assistant answering a question about ONE specific " +
	"codebase. Use ONLY the EVIDENCE provided below — code excerpts, symbol signatures, and graph edges. " +
	"Answer in a few concise sentences, concrete and specific to this code. Cite the locations that support " +
	"each claim inline as `path:line`. If the evidence is insufficient to answer fully, say so in one sentence " +
	"and name the most useful file or symbol to read next. Never invent file paths, identifiers, or APIs that " +
	"do not appear in the evidence."

// buildAnswerUser assembles the user turn: the question, the routed
// intent (so the model knows whether it's explaining behavior, listing
// callers, etc.), and the evidence block.
func buildAnswerUser(question, intent, evidence string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "QUESTION: %s\n", strings.TrimSpace(question))
	if intent != "" {
		fmt.Fprintf(&b, "INTENT: %s\n", intent)
	}
	b.WriteString("\nEVIDENCE:\n")
	b.WriteString(evidence)
	return b.String()
}

// ─── answer cache ─────────────────────────────────────────────────────────
//
// Agents re-ask the same question repeatedly within a session. The
// cache key folds in the evidence text, so a re-index that changes the
// retrieved chunks (or a different model) naturally misses — no explicit
// invalidation needed. Bounded FIFO; correctness doesn't depend on
// retention, only latency/GPU savings.

const answerCacheCap = 256

// AnswerCache is a bounded FIFO string→string cache. Zero value is
// usable; put() allocates the map on first use. Held for the lifetime
// of the owning transport so it persists across requests.
type AnswerCache struct {
	mu    sync.Mutex
	data  map[string]string
	order []string
}

func (c *AnswerCache) key(question, intent, model, evidence string) string {
	h := sha256.New()
	// Length-prefix each field so concatenation can't collide across
	// boundaries (e.g. "ab"+"c" vs "a"+"bc").
	for _, part := range []string{question, intent, model, evidence} {
		h.Write(fmt.Appendf(nil, "%d:", len(part)))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *AnswerCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *AnswerCache) put(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = make(map[string]string, answerCacheCap)
	}
	if _, exists := c.data[key]; exists {
		c.data[key] = val
		return
	}
	if len(c.order) >= answerCacheCap {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.data, oldest)
	}
	c.data[key] = val
	c.order = append(c.order, key)
}
