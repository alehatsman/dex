package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/redact"
)

// CCR — content-addressed recovery for pruned history (#597).
//
// PruneHistory replaces bulky old tool_result content with a lossy stub. CCR
// adds a durable, content-addressed tee store alongside that rewrite: the raw
// bytes are persisted under a hash and the stub carries a recovery marker
// (dex:lc_expand:<hash>). A pruned read is therefore never truly lost — the
// exact original can be reconstructed from the store by hash.
//
// This is the recovery PRIMITIVE (issue #597 v1). Tee-on-prune and the
// ExpandMarkers inverse are both implemented and tested here. The live
// re-injection pass is scoped to the keep-recent window (where v1 pruning
// never writes markers, so it is a correct no-op on normal traffic); the
// active over-pruning recovery that places markers in that window is the
// deferred follow-up (Option 2).
//
// Properties (all from the issue):
//   - Content-addressed: the marker is a pure function of the content, so the
//     same stub always resolves the same way — prompt-cache-safe.
//   - Threshold: originals smaller than ccrMinBytes are not teed (the stub
//     overhead is not worth it).
//   - Redaction on write: credential-shaped substrings are masked before bytes
//     touch disk, same policy as the rest of the proxy.
//   - TTL cleanup: entries older than ccrTTL are GC'd, throttled to one sweep
//     per ccrGCInterval.

const (
	// ccrMinBytes is the smallest original content teed. Below this the stub +
	// marker overhead outweighs any recovery benefit.
	ccrMinBytes = 512
	// ccrTTL is how long a teed entry is retained before GC removes it.
	ccrTTL = 24 * time.Hour
	// ccrGCInterval throttles the GC sweep so it runs at most once per window.
	ccrGCInterval = 600 * time.Second
	// ccrHashLen is the number of hex chars kept from the content hash. 16 hex
	// (64 bits) is ample for a per-session recovery cache and keeps the marker
	// compact.
	ccrHashLen = 16
)

// expandMarkerRe matches an embedded recovery marker and captures its hash.
// The {16} length tracks ccrHashLen (asserted in ccr_test.go).
var expandMarkerRe = regexp.MustCompile(`dex:lc_expand:([0-9a-f]{16})`)

// TeeStore is a content-addressed, on-disk store of pruned tool_result bytes.
// A nil *TeeStore disables CCR entirely: tee-on-prune is skipped and the live
// expansion pass is not run. All methods are safe for concurrent use and
// fail-open — a disk error never propagates into the request path.
type TeeStore struct {
	dir string

	mu     sync.Mutex
	lastGC time.Time
}

// NewTeeStore creates (or reuses) the tee directory under the user cache dir
// (~/.cache/dex/proxy/tee). It returns nil, err on failure so the caller can
// log and run with CCR disabled rather than aborting startup.
func NewTeeStore() (*TeeStore, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cacheDir, "dex", "proxy", "tee")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &TeeStore{dir: dir}, nil
}

// hashContent returns the content-addressed hash of s (redacted-bytes hash).
func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:ccrHashLen]
}

// marker returns the recovery marker token for a hash.
func marker(hash string) string { return "dex:lc_expand:" + hash }

// Put redacts content, stores it under its content hash, and returns the hash.
// Returns ("", false) when ts is nil, the content is below ccrMinBytes, or the
// write fails (fail-open — pruning still proceeds with its lossy stub). The
// stored bytes are the redacted form, and the hash is computed over them, so a
// marker always resolves to exactly what Get returns.
func (ts *TeeStore) Put(content string) (string, bool) {
	if ts == nil || len(content) < ccrMinBytes {
		return "", false
	}
	masked := redact.Mask(content)
	hash := hashContent(masked)
	path := filepath.Join(ts.dir, hash+".log")

	// Already stored (content-addressed) → reuse, just refresh mtime so the
	// TTL clock restarts on re-use.
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return hash, true
	}

	// Atomic write: temp file in the same dir, then rename.
	tmp, err := os.CreateTemp(ts.dir, hash+".*.tmp")
	if err != nil {
		return "", false
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(masked); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}
	return hash, true
}

// Get returns the stored bytes for a hash. The second result is false when ts
// is nil or the entry is absent (expired, never stored, or unreadable).
func (ts *TeeStore) Get(hash string) (string, bool) {
	if ts == nil || hash == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(ts.dir, hash+".log"))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// MaybeGC removes entries older than ccrTTL, but only if at least
// ccrGCInterval has elapsed since the last sweep. Safe to call once per
// request; it self-throttles. No-op on a nil store.
func (ts *TeeStore) MaybeGC() {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	now := time.Now()
	if !ts.lastGC.IsZero() && now.Sub(ts.lastGC) < ccrGCInterval {
		ts.mu.Unlock()
		return
	}
	ts.lastGC = now
	ts.mu.Unlock()

	entries, err := os.ReadDir(ts.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > ccrTTL {
			_ = os.Remove(filepath.Join(ts.dir, e.Name()))
		}
	}
}

// parseExpandMarker returns the hash embedded in text, or "" if none.
func parseExpandMarker(text string) string {
	m := expandMarkerRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

// ExpandMarkers reconstructs pruned content in the keep-recent window: for each
// tool_result there carrying a dex:lc_expand:<hash> marker whose bytes are
// still in the store, the block content is replaced with the recovered bytes.
// Returns the rewritten body and the number of blocks restored.
//
// Scope is deliberate. PruneHistory only writes markers in the OLD region;
// expanding those would undo pruning. Restricting to the keep window (index >=
// pruneStart) makes this a no-op on v1 traffic while positioning the inverse
// pass exactly where the deferred active-recovery trigger (Option 2) will place
// keep-window markers. Fail-open: any parse/marshal error returns the body
// unchanged.
func (ts *TeeStore) ExpandMarkers(body []byte, keepRecent int) ([]byte, int) {
	if ts == nil {
		return body, 0
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body, 0
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, 0
	}
	var messages []json.RawMessage
	if json.Unmarshal(msgsRaw, &messages) != nil {
		return body, 0
	}

	boundary := pruneStart(len(messages), keepRecent)
	restored := 0
	changed := false
	for i := boundary; i < len(messages); i++ {
		rewritten, n := ts.expandMessage(messages[i])
		if n > 0 {
			messages[i] = rewritten
			restored += n
			changed = true
		}
	}
	if !changed {
		return body, 0
	}

	newMsgs, err := json.Marshal(messages)
	if err != nil {
		return body, 0
	}
	raw["messages"] = newMsgs
	out, err := json.Marshal(raw)
	if err != nil {
		return body, 0
	}
	return out, restored
}

// expandMessage restores marked tool_result blocks in one user message.
func (ts *TeeStore) expandMessage(raw json.RawMessage) (json.RawMessage, int) {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw, 0
	}
	var role string
	if r, ok := msg["role"]; !ok || json.Unmarshal(r, &role) != nil || role != "user" {
		return raw, 0
	}
	contentRaw, ok := msg["content"]
	if !ok {
		return raw, 0
	}
	var blocks []json.RawMessage
	if json.Unmarshal(contentRaw, &blocks) != nil {
		return raw, 0 // bare string — no blocks to expand
	}

	restored := 0
	for i, blkRaw := range blocks {
		rewritten, ok := ts.expandBlock(blkRaw)
		if ok {
			blocks[i] = rewritten
			restored++
		}
	}
	if restored == 0 {
		return raw, 0
	}

	newContent, err := json.Marshal(blocks)
	if err != nil {
		return raw, 0
	}
	msg["content"] = newContent
	out, err := json.Marshal(msg)
	if err != nil {
		return raw, 0
	}
	return out, restored
}

// expandBlock replaces a marked tool_result's content with recovered bytes.
func (ts *TeeStore) expandBlock(raw json.RawMessage) (json.RawMessage, bool) {
	var blk map[string]json.RawMessage
	if json.Unmarshal(raw, &blk) != nil {
		return raw, false
	}
	var typ string
	if t, ok := blk["type"]; !ok || json.Unmarshal(t, &typ) != nil || typ != "tool_result" {
		return raw, false
	}
	contentRaw, ok := blk["content"]
	if !ok {
		return raw, false
	}
	hash := parseExpandMarker(extractToolResultText(contentRaw))
	if hash == "" {
		return raw, false
	}
	orig, ok := ts.Get(hash)
	if !ok {
		return raw, false // bytes gone (expired) — leave the marker as-is
	}
	newContent, err := json.Marshal(orig)
	if err != nil {
		return raw, false
	}
	blk["content"] = newContent
	out, err := json.Marshal(blk)
	if err != nil {
		return raw, false
	}
	return out, true
}
