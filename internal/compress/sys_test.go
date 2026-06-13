package compress

import (
	"strings"
	"testing"
)

func TestCompressEnvFilter_RedactsSecretValues(t *testing.T) {
	// Five distinct prefixes → five single-var groups → all rendered in the
	// [other] block (no group truncation), so every value is observable.
	in := []string{
		"DATABASE_URL=postgres://user:pw@host:5432/db", // cred URL: redact password only
		"APP_PASSPHRASE=hunter2secret",                 // key denylist (PASSPHRASE)
		"CLOUD_ID=AKIAIOSFODNN7EXAMPLE",                // secret-shaped value, benign key
		"HOMEPAGE_LINK=https://example.com/docs",       // benign URL: untouched
		"LOG_LEVEL=debug",                              // benign value: passthrough
	}
	out := strings.Join(CompressEnvFilter(in), "\n")

	// Connection-URL password redacted, structure kept useful.
	if !strings.Contains(out, "postgres://user:***@host:5432/db") {
		t.Errorf("DATABASE_URL password not redacted; output:\n%s", out)
	}
	if strings.Contains(out, ":pw@") {
		t.Errorf("raw password leaked in connection URL; output:\n%s", out)
	}

	// PASSPHRASE masked by key name.
	if !strings.Contains(out, "APP_PASSPHRASE=***") || strings.Contains(out, "hunter2secret") {
		t.Errorf("PASSPHRASE value not masked; output:\n%s", out)
	}

	// Secret-shaped value under a non-sensitive key masked via LooksLikeSecret.
	if !strings.Contains(out, "CLOUD_ID=***") || strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("secret-shaped value under benign key not masked; output:\n%s", out)
	}

	// Benign URL (no inline credentials) must survive intact.
	if !strings.Contains(out, "https://example.com/docs") {
		t.Errorf("benign URL was over-masked; output:\n%s", out)
	}

	// Plain value passes through.
	if !strings.Contains(out, "LOG_LEVEL=debug") {
		t.Errorf("benign value mangled; output:\n%s", out)
	}
}

func TestCompressEnvFilter_RedactsCredentialURLWithoutUser(t *testing.T) {
	// redis://:password@host has an empty username — the password must still
	// be redacted (#460).
	out := strings.Join(CompressEnvFilter([]string{"CACHE_BACKEND=redis://:topsecret@127.0.0.1:6379/0"}), "\n")
	if strings.Contains(out, "topsecret") {
		t.Errorf("password leaked for user-less credential URL; output:\n%s", out)
	}
	if !strings.Contains(out, "redis://:***@127.0.0.1:6379/0") {
		t.Errorf("user-less credential URL not redacted as expected; output:\n%s", out)
	}
}
