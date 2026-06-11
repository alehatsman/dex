// Package tokens provides BPE token counting for compression and savings
// accounting. It wraps a real tiktoken encoder (offline, embedded ranks) behind
// a small interface so every "tokens used / saved" number dex reports is honest
// rather than a whitespace-word or bytes/4 guess.
//
// Different LLM families tokenize the same text differently (5-15% variance), so
// counts are family-aware. cl100k is within ~3% of Claude's real tokenizer;
// o200k_base is exact for GPT-4o+; the Gemini correction factor is empirical.
//
// Ported from lean-ctx rust/src/core/tokens.rs.
package tokens

import (
	"hash/fnv"
	"math"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tokenloader "github.com/pkoukk/tiktoken-go-loader"
)

// Family selects the tokenizer used for counting.
type Family int

const (
	// O200kBase is GPT-4o / GPT-4-turbo (tiktoken o200k_base, exact). Default.
	O200kBase Family = iota
	// Cl100k is Claude / Anthropic, approximated via tiktoken cl100k_base (~3%).
	Cl100k
	// Gemini is Google, o200k_base with a 1.08x correction factor.
	Gemini
	// Llama is Llama 3+ / DeepSeek / Qwen / Mistral, approximated via cl100k_base.
	Llama
)

// DefaultFamily is the family used by the package-level Count and by counters
// constructed without an explicit family. o200k_base is the honest accounting
// default (matches lean-ctx COUNTING_FAMILY); #159 layers per-target selection
// on top via NewFor / Detect.
const DefaultFamily = O200kBase

// charsPerTokenEstimate is the heuristic fallback ratio when no BPE encoder is
// available. 3.5 chars/token is closer to real code than the legacy bytes/4.
const charsPerTokenEstimate = 3.5

// geminiCorrection: Gemini tokens run ~8% larger than o200k on average.
const geminiCorrection = 1.08

func (f Family) String() string {
	switch f {
	case O200kBase:
		return "o200k_base"
	case Cl100k:
		return "cl100k_base"
	case Gemini:
		return "gemini"
	case Llama:
		return "llama"
	default:
		return "o200k_base"
	}
}

// encoding returns the underlying tiktoken encoding name for a family.
func (f Family) encoding() string {
	switch f {
	case Cl100k, Llama:
		return "cl100k_base"
	default: // O200kBase, Gemini
		return "o200k_base"
	}
}

// Detect maps a client or model name to a tokenizer family by case-insensitive
// substring. Falls back to DefaultFamily.
func Detect(modelName string) Family {
	l := strings.ToLower(modelName)
	switch {
	case containsAny(l, "claude", "anthropic", "sonnet", "opus", "haiku"):
		return Cl100k
	case containsAny(l, "gemini", "google"):
		return Gemini
	case containsAny(l, "llama", "codex", "opencode", "mistral", "deepseek", "qwen"):
		return Llama
	default:
		return DefaultFamily
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Counter counts tokens in a string.
type Counter interface {
	// Count returns the token count of s.
	Count(s string) int
	// Family reports which tokenizer family produced the counts.
	Family() Family
}

// ── tiktoken loading (offline, embedded ranks, lazy per-encoding) ──

var (
	loaderOnce sync.Once
	bpeMu      sync.Mutex
	bpeCache   = map[string]*tiktoken.Tiktoken{}
	bpeTried   = map[string]bool{}
)

// getBPE lazily loads and memoizes the encoder for an encoding name. Returns nil
// if loading fails (caller falls back to the heuristic). Loads are offline via
// the embedded rank tables — no network at runtime.
func getBPE(encoding string) *tiktoken.Tiktoken {
	loaderOnce.Do(func() {
		tiktoken.SetBpeLoader(tokenloader.NewOfflineLoader())
	})
	bpeMu.Lock()
	defer bpeMu.Unlock()
	if enc, ok := bpeCache[encoding]; ok {
		return enc
	}
	if bpeTried[encoding] {
		return nil
	}
	bpeTried[encoding] = true
	enc, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		return nil
	}
	bpeCache[encoding] = enc
	return enc
}

// ── result cache (bounded, family-keyed) ──

const cacheMax = 8192

var (
	countMu    sync.Mutex
	countCache = map[uint64]int{}
)

func cacheKey(s string, f Family) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64() ^ (uint64(f) << 1)
}

// ── counters ──

type bpeCounter struct {
	family Family
	enc    *tiktoken.Tiktoken
}

func (c *bpeCounter) Family() Family { return c.family }

func (c *bpeCounter) Count(s string) int {
	if s == "" {
		return 0
	}
	key := cacheKey(s, c.family)
	countMu.Lock()
	if n, ok := countCache[key]; ok {
		countMu.Unlock()
		return n
	}
	countMu.Unlock()

	raw := len(c.enc.Encode(s, nil, nil))
	n := raw
	if c.family == Gemini {
		n = int(math.Ceil(float64(raw) * geminiCorrection))
	}

	countMu.Lock()
	if len(countCache) >= cacheMax {
		countCache = map[uint64]int{}
	}
	countCache[key] = n
	countMu.Unlock()
	return n
}

type heuristicCounter struct {
	family Family
}

func (c *heuristicCounter) Family() Family { return c.family }

func (c *heuristicCounter) Count(s string) int {
	if s == "" {
		return 0
	}
	return int(math.Ceil(float64(len([]rune(s))) / charsPerTokenEstimate))
}

// New returns a Counter for the default family.
func New() Counter { return NewFor(DefaultFamily) }

// NewFor returns a Counter for the given family. It uses a real BPE encoder when
// the embedded ranks load, and a chars/token heuristic otherwise so counting
// never hard-fails.
func NewFor(f Family) Counter {
	if enc := getBPE(f.encoding()); enc != nil {
		return &bpeCounter{family: f, enc: enc}
	}
	return &heuristicCounter{family: f}
}

// ── package-level convenience ──

var (
	defaultMu      sync.Mutex
	defaultCounter Counter // nil = not yet initialised
)

func def() Counter {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultCounter == nil {
		defaultCounter = NewFor(DefaultFamily)
	}
	return defaultCounter
}

// SetDefaultFamily replaces the family used by the package-level Count
// function. Intended to be called once at server startup from the active
// context profile before any counting work begins. Safe to call concurrently;
// the new counter takes effect on the next Count call.
func SetDefaultFamily(f Family) {
	defaultMu.Lock()
	defaultCounter = NewFor(f)
	defaultMu.Unlock()
}

// Count returns the token count of s using the default family (o200k_base
// unless overridden via SetDefaultFamily).
func Count(s string) int { return def().Count(s) }

// CountFor returns the token count of s for an explicit family.
func CountFor(s string, f Family) int { return NewFor(f).Count(s) }
