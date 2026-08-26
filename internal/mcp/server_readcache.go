// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alehatsman/dex/internal/compress"
)

// bodyHandle stores the file location of a skeleton-mode @B<n> expansion handle.
type bodyHandle struct {
	relPath   string
	startLine int
	endLine   int
	etag      string // file content hash at the time the handle was issued
}

// registerBodyHandles assigns session-global sequence numbers to a slice of
// BodyEntry handles and stores them in the per-session map. Returns the
// remapped body entries (with session-global N values) and a strings.Replacer
// that maps file-local @B1/@B2/… to the session-global keys, so callers can
// fix up the skeleton text produced by compress.SkeletonPass.
//
// Without session-global numbering, two successive skeleton reads of different
// files both produce @B1, @B2, … and the second registration overwrites the
// first, causing expand=@B1 to resolve against the wrong file.
func (s *Server) registerBodyHandles(sessionID, relPath, etag string, bodies []compress.BodyEntry) (remapped []compress.BodyEntry, r *strings.Replacer) {
	if sessionID == "" || len(bodies) == 0 {
		return bodies, strings.NewReplacer()
	}
	s.bodyHandlesMu.Lock()
	defer s.bodyHandlesMu.Unlock()
	if s.bodyHandles == nil {
		s.bodyHandles = make(map[string]map[string]bodyHandle)
		s.bodyHandlesSeq = make(map[string]int)
	}
	if s.bodyHandles[sessionID] == nil {
		s.bodyHandles[sessionID] = make(map[string]bodyHandle)
	}
	base := s.bodyHandlesSeq[sessionID]
	pairs := make([]string, 0, len(bodies)*2)
	remapped = make([]compress.BodyEntry, len(bodies))
	for i, be := range bodies {
		newN := base + i + 1
		newKey := fmt.Sprintf("@B%d", newN)
		remapped[i] = compress.BodyEntry{N: newN, Name: be.Name, StartLine: be.StartLine, EndLine: be.EndLine}
		s.bodyHandles[sessionID][newKey] = bodyHandle{
			relPath:   relPath,
			startLine: be.StartLine,
			endLine:   be.EndLine,
			etag:      etag,
		}
		// Map file-local key to session-global key.
		pairs = append(pairs, fmt.Sprintf("@B%d", be.N), newKey)
	}
	s.bodyHandlesSeq[sessionID] = base + len(bodies)
	return remapped, strings.NewReplacer(pairs...)
}

// lookupBodyHandle returns the stored body handle for key (e.g. "@B3") in
// sessionID, and a boolean indicating whether it was found.
func (s *Server) lookupBodyHandle(sessionID, key string) (bodyHandle, bool) {
	s.bodyHandlesMu.Lock()
	defer s.bodyHandlesMu.Unlock()
	if s.bodyHandles == nil {
		return bodyHandle{}, false
	}
	h, ok := s.bodyHandles[sessionID][key]
	return h, ok
}

// readCacheCheck returns true when this session has previously received
// relPath at exactly this etag, meaning the model already has the content.
// readCacheCheck returns true when sessionID already received relPath (in the
// given mode) at etag — so the caller can short-circuit with "unchanged".
// mode is included in the key so switching modes on the same file never
// returns "unchanged" when new output is expected (#770).
func (s *Server) readCacheCheck(sessionID, relPath, etag, mode string) bool {
	if sessionID == "" {
		return false
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readCache == nil {
		return false
	}
	return s.readCache[sessionID][relPath+"\x00"+mode] == etag
}

// readCacheMark records that sessionID has received relPath (in mode) at etag.
func (s *Server) readCacheMark(sessionID, relPath, etag, mode string) {
	if sessionID == "" {
		return
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readCache == nil {
		s.readCache = make(map[string]map[string]string)
	}
	if s.readCache[sessionID] == nil {
		s.readCache[sessionID] = make(map[string]string)
	}
	s.readCache[sessionID][relPath+"\x00"+mode] = etag
}

// readCacheGetContent returns the raw file bytes last delivered for (sessionID, relPath).
func (s *Server) readCacheGetContent(sessionID, relPath string) ([]byte, bool) {
	if sessionID == "" {
		return nil, false
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readContentCache == nil {
		return nil, false
	}
	b, ok := s.readContentCache[sessionID][relPath]
	return b, ok
}

// readCacheSetContent records the raw file bytes delivered for (sessionID, relPath).
func (s *Server) readCacheSetContent(sessionID, relPath string, data []byte) {
	if sessionID == "" {
		return
	}
	s.readCacheMu.Lock()
	defer s.readCacheMu.Unlock()
	if s.readContentCache == nil {
		s.readContentCache = make(map[string]map[string][]byte)
	}
	if s.readContentCache[sessionID] == nil {
		s.readContentCache[sessionID] = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	s.readContentCache[sessionID][relPath] = cp
}

// sessionAutoFile records relPath in the active session (if one with a task
// exists) without blocking the caller. Safe to call from any mode.
func (s *Server) sessionAutoFile(dbPath, relPath string) {
	if _, err := os.Stat(dbPath); err != nil {
		return // no index yet — nothing to track
	}
	s.sessionWG.Add(1)
	go func() {
		defer s.sessionWG.Done()
		ctx := context.Background()
		st, err := s.openStore(dbPath)
		if err != nil {
			return
		}
		_ = st.SessionTrackFile(ctx, relPath, "read")
	}()
}

// waitSessionWrites blocks until all in-flight sessionAutoFile goroutines
// finish their store writes. Tests defer it before TempDir cleanup so
// background writes don't race the dir removal.
func (s *Server) waitSessionWrites() { s.sessionWG.Wait() }
