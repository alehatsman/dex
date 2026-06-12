package mcp

// The explore response envelope (#316 story 2): every verb that emits a code
// locator stamps it with an expansion handle so the agent can read the range
// back via read(handle=…) without ever constructing a path:line by hand. The
// stampers below are the single wiring point — handlers build their hit slices
// exactly as before and call one stamper just before returning.

// makeHandle returns an expansion handle for a locator, or "" when the locator
// isn't a real file range worth handing back: pseudo-paths (git-commit hits
// carry start_line 0) and paths that fail the structural guard are skipped so
// read() never receives a handle it is bound to reject.
func makeHandle(path string, start, end int) string {
	if start < 1 || !validateHandlePath(path) {
		return ""
	}
	return EncodeHandle(path, start, end)
}

func stampSearchHandles(hits []SearchHit) {
	for i := range hits {
		hits[i].Handle = makeHandle(hits[i].Path, hits[i].StartLine, hits[i].EndLine)
	}
}

func stampSemHandles(hits []SemHit) {
	for i := range hits {
		hits[i].Handle = makeHandle(hits[i].Path, hits[i].StartLine, hits[i].EndLine)
	}
}

func stampSymbolHandles(hits []SymbolHit) {
	for i := range hits {
		hits[i].Handle = makeHandle(hits[i].Path, hits[i].StartLine, hits[i].EndLine)
	}
}

func stampReadHandles(reads []SuggestedRead) {
	for i := range reads {
		reads[i].Handle = makeHandle(reads[i].Path, reads[i].StartLine, reads[i].EndLine)
	}
}
