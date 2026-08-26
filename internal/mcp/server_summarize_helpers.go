package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadModes returns the user-facing `read` modes the Summarize dispatch above
// handles, in documentation order. It is the canonical anchor for CLI↔MCP mode
// parity (cmd/dex/read_parity_test.go) — keep it in sync with the switch.
//
// `handle` is intentionally excluded: it is an internal terminal of the
// budget-downgrade chain (#487), never a mode an operator selects. `lines` is
// listed as a stand-in for the `lines:N-M` prefix family.
func ReadModes() []string {
	all := AllReadModes()
	out := make([]string, len(all))
	for i, m := range all {
		out[i] = string(m)
	}
	return out
}

// applyExpansionHandle decodes an in.Handle (#344) into a concrete path + line
// range, superseding any path/paths/lines the caller also passed. It returns
// the updated input, or a non-nil *SummarizeOutput when the handle is malformed
// (the caller should return it as-is). A blank handle is a no-op.
func applyExpansionHandle(in SummarizeInput) (SummarizeInput, *SummarizeOutput) {
	h := strings.TrimSpace(in.Handle)
	if h == "" {
		return in, nil
	}
	path, start, end, ok := DecodeHandle(h)
	if !ok {
		return in, &SummarizeOutput{Status: "bad-handle", Hint: "handle did not decode to a valid path:line range; re-run find/ask to get a fresh handle"}
	}
	in.Path = path
	in.StartLine = start
	in.EndLine = end
	in.Paths = nil
	if strings.TrimSpace(in.Mode) == "" {
		in.Mode = fmt.Sprintf("lines:%d-%d", start, end)
	}
	return in, nil
}

// summarizePreFile is the combined pre-file-read gate (#344, #630).
// It handles CCR blob expansion (ccr_hash present → return immediately) and
// expansion handle decoding (handle present → rewrite path/range). A non-nil
// *SummarizeOutput means the caller should return it unchanged.
func (s *Server) summarizePreFile(in SummarizeInput) (SummarizeInput, *SummarizeOutput) {
	if strings.TrimSpace(in.CCRHash) != "" {
		out := s.summarizeCCR(in)
		return in, &out
	}
	return applyExpansionHandle(in)
}

// summarizeModeSlice applies a Slice spec to file content and signals done=true
// so the caller can bypass mode dispatch (#630). Returns done=false when no
// Slice is set, letting the normal mode switch handle the request.
func (s *Server) summarizeModeSlice(
	in SummarizeInput, data []byte, etag, sessionID, relTarget string, out SummarizeOutput,
) (earlyOut SummarizeOutput, done bool) {
	spec := strings.TrimSpace(in.Slice)
	if spec == "" {
		return SummarizeOutput{}, false
	}
	sliced, hint, err := applySlice(data, spec)
	if err != nil {
		return SummarizeOutput{Status: "error", Hint: fmt.Sprintf("slice: %v", err)}, true
	}
	out.Status = "ok"
	out.Etag = etag
	out.Content = string(sliced)
	out.Bytes = len(sliced)
	out.Hint = hint
	s.readCacheMark(sessionID, relTarget, etag, in.Mode)
	return out, true
}

// resolveProjectRoot returns projectRoot if non-empty; otherwise it consults the
// client's declared workspace roots (#120) and falls back to the server cwd.
// Same precedence as resolveProject, but yields the raw path summarize needs.
// The error message is already user-facing.
func (s *Server) resolveProjectRoot(ctx context.Context, projectRoot string) (string, error) {
	if projectRoot != "" {
		return projectRoot, nil
	}
	if l := listerFromContext(ctx); l != nil {
		if r := rootFromClient(ctx, l, s.IndexDir); r != "" {
			return r, nil
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", errors.New("could not determine project root; pass project_root explicitly")
	}
	warnCwdFallback(wd)
	return wd, nil
}

// summarizeCCR retrieves a content-addressed blob from the proxy's CCR tee
// store (#630). It applies Slice if set and returns the blob as plain content.
func (s *Server) summarizeCCR(in SummarizeInput) SummarizeOutput {
	hash := strings.TrimSpace(in.CCRHash)
	content, ok := s.ccrGet(hash)
	if !ok {
		return SummarizeOutput{
			Status: "not-found",
			Hint:   fmt.Sprintf("CCR hash %q not found — the blob may have expired (TTL 24h) or the proxy CCR store is not enabled (DEX_PROXY_CCR)", hash),
		}
	}
	if spec := strings.TrimSpace(in.Slice); spec != "" {
		sliced, hint, err := applySlice([]byte(content), spec)
		if err != nil {
			return SummarizeOutput{Status: "error", Hint: fmt.Sprintf("slice: %v", err)}
		}
		return SummarizeOutput{
			Status:  "ok",
			Content: string(sliced),
			Bytes:   len(sliced),
			Hint:    fmt.Sprintf("CCR blob %s: %s", hash, hint),
		}
	}
	return SummarizeOutput{
		Status:  "ok",
		Content: content,
		Bytes:   len(content),
		Hint:    fmt.Sprintf("CCR blob %s", hash),
	}
}

// ccrGet reads a content-addressed blob from the proxy's tee store directory.
// Returns ("", false) when the hash is absent, expired, or the dir is unreadable.
// The dir is always ~/.cache/dex/proxy/tee (same path as internal/proxy.TeeStore).
func (s *Server) ccrGet(hash string) (string, bool) {
	if hash == "" {
		return "", false
	}
	dir := s.CCRDir
	if dir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "", false
		}
		dir = filepath.Join(cacheDir, "dex", "proxy", "tee")
	}
	b, err := os.ReadFile(filepath.Join(dir, hash+".log"))
	if err != nil {
		return "", false
	}
	return string(b), true
}
