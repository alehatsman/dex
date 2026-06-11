package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestNavTierFiltering(t *testing.T) {
	tests := []struct {
		tier    toolTier
		wantAsk bool
		wantNav bool
		wantFT  bool // file_tree (standard)
		wantSS  bool // search_semantic (power)
	}{
		{TierAsk, true, false, false, false},
		{TierStandard, true, true, true, false},
		{TierPower, true, true, true, true},
	}
	for _, tc := range tests {
		entries := navEntriesForTier(tc.tier)
		names := make(map[string]bool, len(entries))
		for _, e := range entries {
			names[e.Name] = true
		}
		if names["ask"] != tc.wantAsk {
			t.Errorf("tier=%d ask=%v want=%v", tc.tier, names["ask"], tc.wantAsk)
		}
		if names["ctx_nav"] != tc.wantNav {
			t.Errorf("tier=%d ctx_nav=%v want=%v", tc.tier, names["ctx_nav"], tc.wantNav)
		}
		if names["file_tree"] != tc.wantFT {
			t.Errorf("tier=%d file_tree=%v want=%v", tc.tier, names["file_tree"], tc.wantFT)
		}
		if names["search_semantic"] != tc.wantSS {
			t.Errorf("tier=%d search_semantic=%v want=%v", tc.tier, names["search_semantic"], tc.wantSS)
		}
	}
}

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
	// ctx_nav itself should appear in the list for standard tier
	var found bool
	for _, e := range out.Tools {
		if e.Name == "ctx_nav" {
			found = true
			if e.Tier != "standard" {
				t.Errorf("ctx_nav.Tier=%q want standard", e.Tier)
			}
		}
	}
	if !found {
		t.Error("ctx_nav not in tools list")
	}
}

func TestNavTextContainsTools(t *testing.T) {
	out := NavOutput{
		Status: "ok",
		Guide:  "guide text",
		Tools: []NavEntry{
			{Name: "ask", Tier: "all", Purpose: "p1", When: "w1"},
			{Name: "file_tree", Tier: "standard", Purpose: "p2", When: "w2"},
		},
	}
	text := navText(out)
	if !strings.Contains(text, "### ask") {
		t.Error("text missing '### ask'")
	}
	if !strings.Contains(text, "### file_tree") {
		t.Error("text missing '### file_tree'")
	}
}
