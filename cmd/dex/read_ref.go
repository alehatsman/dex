package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
	"github.com/alehatsman/dex/internal/source"
)

// entropyFiltered drops low-information lines from text (entropy mode), keeping
// the original lines when the filter would strip everything.
func entropyFiltered(text string) string {
	ranged := strings.Split(text, "\n")
	filtered := compress.EntropyFilter(ranged, compress.EntropyThresholdStandard)
	if filtered == nil {
		return strings.Join(ranged, "\n")
	}
	return strings.Join(filtered, "\n")
}

// maybeCompactJSON losslessly compacts whole-file JSON/JSONL content, stripping
// insignificant whitespace with zero semantic loss (#619). Returns the input
// unchanged for non-JSON extensions or unparseable content.
func maybeCompactJSON(content, ext string) string {
	switch strings.ToLower(ext) {
	case ".jsonl", ".ndjson":
		if c, ok := compress.CompactJSONL(content); ok {
			return c
		}
	case ".json":
		if c, ok := compress.CompactJSON(content); ok {
			return c
		}
	}
	return content
}

// readSource returns the file content from the working tree, or from a git ref
// when ref is non-empty (#656, via the shared source.ReadAtRef helper #657).
func readSource(ctx context.Context, path, ref string) ([]byte, error) {
	if ref != "" {
		return source.ReadAtRef(ctx, path, ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}
