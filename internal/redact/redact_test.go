package redact

import (
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		hidden string // substring that must NOT survive
		keep   string // substring that must survive (prefix/context)
	}{
		{"bearer", "Authorization: Bearer abcdef1234567890", "abcdef1234567890", "Bearer "},
		{"api key", "api_key=supersecretvalue1234", "supersecretvalue1234", "api_key="},
		{"password", "password: hunter2hunter2hunter2", "hunter2hunter2hunter2", "password:"},
		{"aws", "id AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE", "here"},
		{"github", "token ghp_abcdefghijklmnopqrstuvwxyz0123", "abcdefghijklmnopqrstuvwxyz0123", "ghp_"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Mask(c.in)
			if strings.Contains(got, c.hidden) {
				t.Errorf("secret survived: %q -> %q", c.in, got)
			}
			if c.keep != "" && !strings.Contains(got, c.keep) {
				t.Errorf("context lost: %q -> %q (want substring %q)", c.in, got, c.keep)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("no redaction marker: %q -> %q", c.in, got)
			}
		})
	}
}

func TestMask_NoFalsePositive(t *testing.T) {
	clean := "the quick brown fox ran 12 miles to file.go:42"
	if got := Mask(clean); got != clean {
		t.Errorf("clean text altered: %q -> %q", clean, got)
	}
}
