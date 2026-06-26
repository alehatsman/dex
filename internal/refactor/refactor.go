// Package refactor plans type-precise source edits for an agent to apply.
//
// It is the engine behind the `refactor` verb (#638 / GitHub #65 Tier S3). dex
// is read-only by design (#551): refactor never writes files — it returns a set
// of (path, start_byte, end_byte, replacement) edit triples that the host agent
// applies with its own Edit tool. v1 supports only rename_symbol for Go, planned
// on-demand via a go/packages + go/types load (the x/tools/refactor/rename
// technique) rather than the persisted graph, which carries declaration byte
// spans but no per-reference byte data.
package refactor

// EditTriple is one type-precise replacement: bytes [StartByte, EndByte) in Path
// become Replacement. Offsets are 0-based byte offsets into the file's current
// content; Line is the 1-based line for human display only.
type EditTriple struct {
	Path        string `json:"path"` // project-relative, slash-separated
	StartByte   int    `json:"start_byte"`
	EndByte     int    `json:"end_byte"`
	Replacement string `json:"replacement"`
	Line        int    `json:"line,omitempty"`
}

// RenameResult is the plan for a rename_symbol operation. Status is the contract:
//   - "ok"                    edits is the full, type-resolved set
//   - "unsupported-language"  no Go module / not a Go symbol
//   - "not-found"             no symbol matched
//   - "ambiguous"             the name matched more than one symbol (see Hint)
//   - "stale"                 caller's Etag != the current file set's Etag
//   - "error"                 load / internal failure (see Hint)
type RenameResult struct {
	Status   string       `json:"status"`
	Hint     string       `json:"hint,omitempty"`
	From     string       `json:"from,omitempty"`
	To       string       `json:"to,omitempty"`
	Object   string       `json:"object,omitempty"` // resolved target, e.g. "method (*Server).Run"
	Edits    []EditTriple `json:"edits,omitempty"`
	Files    int          `json:"files,omitempty"` // distinct files touched
	Etag     string       `json:"etag,omitempty"`  // hash of the touched files' current content
	Warnings []string     `json:"warnings,omitempty"`
}
