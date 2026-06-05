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

func TestExpandSPLADE(t *testing.T) {
	cases := []struct {
		token    string
		camel    string
		wantSubs []string // substrings that must appear in result
	}{
		// known token: wraps with synonyms
		{"auth", `"auth"`, []string{`"auth"`, `"authentication"`, `"jwt"`, `"credential"`}},
		// known token: camel expression preserved as-is inside the group
		{"db", `"db"`, []string{`"db"`, `"database"`, `"sql"`, `"transaction"`}},
		// unknown token: returned unchanged
		{"foobar", `"foobar"`, []string{`"foobar"`}},
	}
	for _, c := range cases {
		got := expandSPLADE(c.token, c.camel)
		for _, want := range c.wantSubs {
			if !containsStr(got, want) {
				t.Errorf("expandSPLADE(%q, %q) = %q, missing %q", c.token, c.camel, got, want)
			}
		}
		// unknown token must return the camel expression unchanged
		if _, ok := spladesExpansions[c.token]; !ok && got != c.camel {
			t.Errorf("expandSPLADE(%q): no expansion expected, got %q", c.token, got)
		}
	}
}

func TestBuildFTSQuerySPLADE(t *testing.T) {
	// "auth" should produce a query containing synonyms.
	q := buildFTSQuery("auth middleware", FTSModeAuto)
	for _, want := range []string{"auth", "authentication", "jwt", "middleware", "interceptor"} {
		if !containsStr(q, want) {
			t.Errorf("buildFTSQuery(auth middleware) = %q, missing %q", q, want)
		}
	}

	// "db migration" should expand both terms.
	q2 := buildFTSQuery("db migration", FTSModeAuto)
	for _, want := range []string{"db", "database", "sql"} {
		if !containsStr(q2, want) {
			t.Errorf("buildFTSQuery(db migration) = %q, missing %q", q2, want)
		}
	}

	// token not in dict: no extra synonyms, original preserved.
	q3 := buildFTSQuery("foobar", FTSModeAuto)
	if q3 != `"foobar"` {
		t.Errorf("buildFTSQuery(foobar) = %q, want %q", q3, `"foobar"`)
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
