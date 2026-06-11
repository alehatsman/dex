package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestNavHandlerReturnsGuide(t *testing.T) {
	s := &Server{}
	_, out, err := s.nav(context.Background(), nil, NavInput{})
	if err != nil {
		t.Fatalf("nav: %v", err)
	}
	if out.Status != "ok" {
		t.Errorf("status=%q want ok", out.Status)
	}
	if !strings.Contains(out.Guide, "ask first") {
		t.Errorf("guide missing 'ask first'; got: %.100s", out.Guide)
	}
	if len(out.Tools) == 0 {
		t.Error("tools list is empty")
	}
	var found bool
	for _, e := range out.Tools {
		if e.Name == "ask" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ask not in tools list")
	}
}

func TestNavTextContainsTools(t *testing.T) {
	out := NavOutput{
		Status: "ok",
		Guide:  "guide text",
		Tools: []NavEntry{
			{Name: "ask", Purpose: "p1", When: "w1"},
			{Name: "ls", Purpose: "p2", When: "w2"},
		},
	}
	text := navText(out)
	if !strings.Contains(text, "### ask") {
		t.Error("text missing '### ask'")
	}
	if !strings.Contains(text, "### ls") {
		t.Error("text missing '### ls'")
	}
}
