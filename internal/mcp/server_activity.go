// Package mcp wires the dex toolset onto the official MCP Go SDK
// and runs it over stdio.
package mcp

import (
	"fmt"
	"sync"
	"time"

	"github.com/alehatsman/dex/internal/throttle"
)

type throttleEntry struct {
	count  int
	lastAt time.Time
}

type activityState struct {
	mu               sync.Mutex
	weightedScore    int
	significantCalls int // calls with weight >= 2
	lastKnowledgeAt  time.Time
	lastNudgeAt      time.Time
}

// activityRecord adds weight for a tool call. weight >= 2 counts as significant.
func (s *Server) bt() *bounceTracker {
	s.bounceOnce.Do(func() { s.bounce = newBounceTracker() })
	return s.bounce
}

func (s *Server) ld() *throttle.Detector {
	s.loopOnce.Do(func() { s.loop = throttle.New() })
	return s.loop
}

func (s *Server) activityRecord(project string, weight int) {
	raw, _ := s.activityTracker.LoadOrStore(project, &activityState{})
	a, ok := raw.(*activityState)
	if !ok {
		return
	}
	a.mu.Lock()
	a.weightedScore += weight
	if weight >= 2 {
		a.significantCalls++
	}
	a.mu.Unlock()
}

// activityKnowledgeRecorded resets the nudge clock when the agent stores a fact.
func (s *Server) activityKnowledgeRecorded(project string) {
	raw, _ := s.activityTracker.LoadOrStore(project, &activityState{})
	a, ok := raw.(*activityState)
	if !ok {
		return
	}
	a.mu.Lock()
	a.lastKnowledgeAt = time.Now()
	a.mu.Unlock()
}

// activityNudge returns a hint when the agent has done substantial work without
// recording findings. sessionTask makes the message context-sensitive. Returns ""
// when below threshold or the nudge was already shown recently.
func (s *Server) activityNudge(project, sessionTask string) string {
	const (
		scoreThreshold = 20
		callThreshold  = 5
		quietWindow    = 8 * time.Minute
	)
	raw, loaded := s.activityTracker.Load(project)
	if !loaded {
		return ""
	}
	a, ok := raw.(*activityState)
	if !ok {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.weightedScore < scoreThreshold || a.significantCalls < callThreshold {
		return ""
	}
	now := time.Now()
	if !a.lastKnowledgeAt.IsZero() && now.Sub(a.lastKnowledgeAt) < quietWindow {
		return ""
	}
	if !a.lastNudgeAt.IsZero() && now.Sub(a.lastNudgeAt) < quietWindow {
		return ""
	}
	a.lastNudgeAt = now
	if sessionTask != "" {
		return fmt.Sprintf("You've done significant work on %q — consider recording key findings with knowledge action=add so they survive a session reset.", sessionTask)
	}
	return "Significant activity detected — consider recording key findings with knowledge action=add so they survive a session reset."
}
