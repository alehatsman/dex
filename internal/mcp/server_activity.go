// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"time"

	"github.com/alehatsman/dex/internal/throttle"
)

type throttleEntry struct {
	count  int
	lastAt time.Time
}

// bt lazily builds the per-server bounce tracker (repeated-read guard).
func (s *Server) bt() *bounceTracker {
	s.bounceOnce.Do(func() { s.bounce = newBounceTracker() })
	return s.bounce
}

// ld lazily builds the per-server loop detector (repeated-search guard).
func (s *Server) ld() *throttle.Detector {
	s.loopOnce.Do(func() { s.loop = throttle.New() })
	return s.loop
}
