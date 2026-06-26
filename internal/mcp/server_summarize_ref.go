package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/ignore"
	"github.com/alehatsman/dex/internal/profiles"
	"github.com/alehatsman/dex/internal/source"
	"github.com/alehatsman/dex/internal/summarize"
)

// setSummarizeModel stamps the chat endpoint/model onto an LLM (summary-mode)
// read's output. No-op for the no-LLM modes.
func (s *Server) setSummarizeModel(out *SummarizeOutput, isLLM bool) {
	if isLLM {
		out.Endpoint = s.ChatClient.Endpoint()
		out.Model = s.ChatClient.ModelName()
	}
}

// summarizePostRead handles the early exits that fire right after a file's
// content is read but before the cache/SLO/bounce machinery: a --ref
// time-travel read (#657) and a binary-file refusal (#674). It returns
// done=true with the output to send when either applies; the caller stamps
// the project root. Keeping both branches here keeps the summarize dispatcher
// under the complexity cap.
func (s *Server) summarizePostRead(ctx context.Context, in SummarizeInput, realTarget, relTarget string, data []byte, mode ReadMode) (SummarizeOutput, bool) {
	if strings.TrimSpace(in.Ref) != "" {
		return s.summarizeRefRead(ctx, in, realTarget, relTarget, mode), true
	}
	// A binary file can't be meaningfully read in any text mode. dex skips
	// binaries at index time; mirror that at read time and refuse with a clear
	// status rather than dumping raw bytes (null bytes included) into the
	// agent's context — pure token waste and potential transport corruption.
	if ignore.LooksBinary(data) {
		return SummarizeOutput{
			Status: "binary",
			Path:   relTarget,
			Bytes:  len(data),
			Hint:   fmt.Sprintf("binary file (%d bytes) — not shown; dex does not read binary content", len(data)),
		}, true
	}
	return SummarizeOutput{}, false
}

// summarizeRefRead serves a read of realTarget as of in.Ref (#657 time-travel).
// Content comes from git, not the working tree, so it supports only the
// content-based modes: full (raw, with an optional line range) and signatures
// (tree-sitter compression of the historical content — the file's API as of the
// ref). The index-backed modes (skeleton/map/summary) describe the HEAD index
// and are rejected. Bypasses the cache/SLO/bounce path entirely: a contained,
// opt-in branch that leaves working-tree reads byte-identical.
func (s *Server) summarizeRefRead(ctx context.Context, in SummarizeInput, realTarget, relTarget string, mode ReadMode) SummarizeOutput {
	data, err := source.ReadAtRef(ctx, realTarget, in.Ref)
	if err != nil {
		return SummarizeOutput{Status: "error", Path: relTarget, Hint: err.Error()}
	}
	if ignore.LooksBinary(data) {
		// A historical version can be binary too — refuse rather than dump raw
		// bytes (#674).
		return SummarizeOutput{Status: "binary", Path: relTarget, Bytes: len(data),
			Hint: fmt.Sprintf("binary file (%d bytes) — not shown; dex does not read binary content", len(data))}
	}
	sliceB, _, _ := summarize.SliceLines(data, in.StartLine, in.EndLine)
	slice := string(sliceB)

	var content string
	switch mode {
	case ReadModeFull:
		content = slice
	case ReadModeSignatures:
		ext := filepath.Ext(relTarget)
		strict := profiles.Active(filepath.Dir(realTarget)).StrictAnchors()
		content = compress.CompressCode(slice, ext, strict)
	default:
		return SummarizeOutput{Status: "error", Path: relTarget,
			Hint: fmt.Sprintf("read ref=%s does not support mode %q (it reads the HEAD index); use full or signatures", in.Ref, mode)}
	}

	sum := sha256.Sum256([]byte(content))
	return SummarizeOutput{
		Status:    "ok",
		Path:      relTarget,
		StartLine: in.StartLine,
		EndLine:   in.EndLine,
		Bytes:     len(content),
		Content:   content,
		Etag:      hex.EncodeToString(sum[:])[:16],
	}
}
