package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/heatmap"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/proj"
	"github.com/alehatsman/dex/internal/slo"
	"github.com/alehatsman/dex/internal/summarize"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// escalateOnBounce re-escalates a compressed read to the complete view when the
// bounce tracker detects the agent re-reading the same file (#98): the LLM
// summary when a chat model is wired, else the raw full file. Already-complete
// modes (full/summary) are left untouched. `skeleton` is deterministic by
// contract (no chat model), so it escalates to raw `full`, never an LLM summary
// (#483). `analyze` is a meta-mode (token-cost comparison, no file content)
// that must never call the LLM regardless of bounce history (#752). `map` is
// an explicit index view (imports+exports), never a compressed substitute for
// full content — bouncing it to LLM summary violates the mode contract (#802).
func (s *Server) escalateOnBounce(bt *bounceTracker, sessionID, relTarget string, mode ReadMode, isLLM bool) (ReadMode, bool) {
	if !bt.shouldForceFull(sessionID, relTarget) || mode.IsComplete() {
		return mode, isLLM
	}
	if mode == ReadModeAnalyze || mode == ReadModeMap {
		return mode, isLLM
	}
	if mode == ReadModeSkeleton {
		return ReadModeFull, false
	}
	if s.ChatClient != nil {
		return ReadModeSummary, true
	}
	return ReadModeFull, false
}

// summarizeResolveMode picks the read mode, applying profile defaults and
// task-aware overrides. The default `full` is raw file content (no LLM); the
// only LLM path is `summary` (isLLM). The no-chat handling for summary lives
// in the caller, which degrades it to a `needs-chat` status.
func (s *Server) summarizeResolveMode(in SummarizeInput) (ReadMode, bool) {
	raw := strings.ToLower(strings.TrimSpace(in.Mode))
	if raw == "" {
		if in.ProjectRoot != "" {
			if prof := profiles.Active(in.ProjectRoot); prof.Read.DefaultMode != "" {
				raw = prof.Read.DefaultMode
			}
		}
		if raw == "" {
			raw = string(ReadModeFull)
		}
	}
	mode := ReadMode(raw)
	isLLM := mode == ReadModeSummary
	// A task hint compresses the raw default toward a structural mode (e.g. a
	// Generate task → signatures) to save tokens; it never forces the LLM.
	if in.Task != "" && mode == ReadModeFull {
		if override := compress.TaskToMode(in.Task); override != "" {
			if p2, h2 := s.resolveProject(in.ProjectRoot); h2 == "" {
				pt := compress.LoadPolicy(p2.CacheDir)
				override = pt.ChooseMode(compress.IntentFromTask(in.Task), override)
			}
			mode = ReadMode(override)
		}
	}
	return mode, isLLM
}

// escapesRoot reports whether path lies outside root. Both are compared
// lexically via filepath.Rel; a relative result of ".." or one prefixed with
// "../" means path climbs above root. The separator-aware prefix avoids a
// false positive on sibling names like "..foo".
func escapesRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// summarizeReadFile resolves target under p, validates it stays inside the
// project root, stats it, reads it, and computes its etag. Returns done=true
// and a populated earlyOut for all error paths.
func (s *Server) summarizeReadFile(p *proj.Project, target string) (
	realTarget, relTarget string, data []byte, etag string, earlyOut SummarizeOutput, done bool,
) {
	if !filepath.IsAbs(target) {
		target = filepath.Join(p.Root, target)
	}
	target = filepath.Clean(target)
	// Containment is decided lexically, before any existence check: an escaping
	// path must be rejected the same way whether or not it happens to exist, so
	// a non-existent escaping path (e.g. ../../etc/shadow) reports "outside
	// project root" rather than leaking the resolved path through a misleading
	// "file does not exist" message (#508). The post-symlink check below then
	// catches in-root paths that symlink out.
	if escapesRoot(p.Root, target) {
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("path %s is outside project root %s", target, p.Root)}, true
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file does not exist: %s", target)}, true
		}
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("resolve path: %v", err)}, true
	}
	rel, err := filepath.Rel(p.Root, real)
	if err != nil || escapesRoot(p.Root, real) {
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("path %s is outside project root %s", target, p.Root)}, true
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("stat: %v", err)}, true
	}
	if fi.IsDir() {
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("%s is a directory — pass a file path", rel)}, true
	}
	d, err := os.ReadFile(real)
	if err != nil {
		return "", "", nil, "", SummarizeOutput{Status: "error", Hint: fmt.Sprintf("read: %v", err)}, true
	}
	h := sha256.Sum256(d)
	return real, rel, d, hex.EncodeToString(h[:])[:16], SummarizeOutput{}, false
}

// summarizeCheckCached extracts the session ID, then returns early when the
// content is unchanged (etag match) or a compact delta can be produced.
// Also sets up the content cache for future delta re-reads.
func (s *Server) summarizeCheckCached(
	req *sdk.CallToolRequest, in SummarizeInput,
	relTarget, etag string, isFull bool, data []byte, out SummarizeOutput,
) (sessionID string, earlyOut SummarizeOutput, done bool) {
	sessionID = "stdio"
	if req != nil && req.Session != nil {
		if id := req.Session.ID(); id != "" {
			sessionID = id
		}
	}
	if in.Etag != "" && in.Etag == etag && s.readCacheCheck(sessionID, relTarget, etag, in.Mode) {
		return sessionID, SummarizeOutput{Status: "unchanged", Project: out.Project, Path: relTarget, Etag: etag}, true
	}
	hasLineRange := in.StartLine != 0 || in.EndLine != 0 || in.Slice != ""
	if in.Expand == "" && !hasLineRange && !(isFull && s.ChatClient != nil) {
		if prevData, ok := s.readCacheGetContent(sessionID, relTarget); ok {
			if delta, worth := computeLineDelta(prevData, data); worth {
				out.Status = "delta"
				out.Etag = etag
				out.Bytes = len(data)
				out.Content = delta
				s.readCacheMark(sessionID, relTarget, etag, in.Mode)
				s.readCacheSetContent(sessionID, relTarget, data)
				return sessionID, out, true
			}
		}
	}
	return sessionID, SummarizeOutput{}, false
}

// summarizeExpandHandle handles @B<n> body handle expansion. Returns done=true
// when a handle was present (regardless of success/error).
func (s *Server) summarizeExpandHandle(
	in SummarizeInput, data []byte, etag, sessionID, relTarget string, out SummarizeOutput,
) (earlyOut SummarizeOutput, done bool) {
	if in.Expand == "" {
		return SummarizeOutput{}, false
	}
	h, ok := s.lookupBodyHandle(sessionID, in.Expand)
	if !ok {
		return SummarizeOutput{Status: "error", Hint: fmt.Sprintf("unknown handle %q — issue a skeleton-mode read first", in.Expand)}, true
	}
	if h.etag != etag {
		return SummarizeOutput{Status: "error", Hint: fmt.Sprintf("file has changed since handle %q was issued — re-read with mode=skeleton", in.Expand)}, true
	}
	slice, sliceStart, sliceEnd := summarize.SliceLines(data, h.startLine, h.endLine)
	out.Status = "ok"
	out.Etag = etag
	out.StartLine = sliceStart
	out.EndLine = sliceEnd
	out.Bytes = len(slice)
	out.Content = string(slice)
	out.Hint = fmt.Sprintf("body expansion of %s (lines %d-%d)", in.Expand, sliceStart, sliceEnd)
	s.readCacheMark(sessionID, relTarget, etag, in.Mode)
	return out, true
}

// recordSummarizeMetrics is called via defer to record heatmap access and SLO
// token usage after a successful summarize call.
func (s *Server) recordSummarizeMetrics(cacheDir string, sloTracker *slo.Tracker, relTarget string, out *SummarizeOutput) {
	if out.Status != "ok" {
		return
	}
	hm := heatmap.Load(cacheDir)
	origTok := out.Bytes / 4
	compTok := len(out.Content) / 4
	saved := origTok - compTok
	if saved < 0 {
		saved = 0
	}
	hm.RecordAccess(relTarget, origTok, saved)
	_ = hm.Save(cacheDir)
	sloTracker.RecordTokens(len(out.Content) / 4)
	if ann := sloAnnotation(sloTracker.Check()); ann != "" {
		if out.Hint == "" {
			out.Hint = ann
		} else {
			out.Hint += " " + ann
		}
	}
}
