package store

import (
	"testing"
)

func TestSplitCamelCase(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"AuthService", []string{"Auth", "Service"}},
		{"handleRequest", []string{"handle", "Request"}},
		{"parseHTTPURL", []string{"parse", "HTTPURL"}},
		{"simple", nil},
		{"AB", nil}, // no lowercase→uppercase transition
		{"AuthMiddlewareHandler", []string{"Auth", "Middleware", "Handler"}},
	}
	for _, c := range cases {
		got := splitCamelCase(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitCamelCase(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitCamelCase(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestExpandCamelTerm(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"AuthService"`, `("AuthService" OR "Auth" OR "Service")`},
		{`"simple"`, `"simple"`},                           // no expansion
		{`"auth service"`, `"auth service"`},                // phrase — no expansion
		{`"AuthMiddlewareHandler"`, `("AuthMiddlewareHandler" OR "Auth" OR "Middleware" OR "Handler")`},
	}
	for _, c := range cases {
		got := expandCamelTerm(c.in)
		if got != c.want {
			t.Errorf("expandCamelTerm(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildFTSQueryCamelExpansion(t *testing.T) {
	// A CamelCase token should expand to include sub-tokens.
	q := buildFTSQuery("AuthService", FTSModeAuto)
	if q == "" {
		t.Fatal("expected non-empty query")
	}
	// Should contain the original and at least one sub-token.
	if !contains(q, "AuthService") {
		t.Errorf("query %q missing original token AuthService", q)
	}
	if !contains(q, "Auth") {
		t.Errorf("query %q missing sub-token Auth", q)
	}
	if !contains(q, "Service") {
		t.Errorf("query %q missing sub-token Service", q)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
