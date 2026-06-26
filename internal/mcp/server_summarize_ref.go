package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/alehatsman/dex/internal/compress"
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
