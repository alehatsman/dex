package compress

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	// 36 alnum chars → matches the classic GitHub PAT pattern.
	ghToken := "ghp_" + strings.Repeat("a", 36)

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "credential URL password span",
			in:   []string{"DATABASE_URL=postgres://admin:s3cr3t@db:5432/app"},
			want: []string{"DATABASE_URL=postgres://admin:***@db:5432/app"},
		},
		{
			name: "credential URL empty username",
			in:   []string{"redis://:hunter2@cache:6379"},
			want: []string{"redis://:***@cache:6379"},
		},
		{
			name: "kubectl secret yaml base64 value under password key",
			in:   []string{"  password: cGFzc3dvcmQ="},
			want: []string{"  password: ***"},
		},
		{
			name: "env-style sensitive key assignment",
			in:   []string{"API_KEY=abcdef0123456789"},
			want: []string{"API_KEY=***"},
		},
		{
			name: "recognizable token under benign key",
			in:   []string{"note: see " + ghToken + " for access"},
			want: []string{"note: see *** for access"},
		},
		{
			name: "bare auth-flow token line",
			in:   []string{ghToken},
			want: []string{"***"},
		},
		{
			name: "table/list marker before sensitive key",
			in:   []string{"| token | " + ghToken + " |"},
			want: []string{"| token | *** |"},
		},
		{
			name: "non-secret content unchanged",
			in: []string{
				"INFO  starting server on :8080",
				"PASS  pkg/foo  0.21s",
				"export PATH=/usr/bin:/bin",
			},
			want: []string{
				"INFO  starting server on :8080",
				"PASS  pkg/foo  0.21s",
				"export PATH=/usr/bin:/bin",
			},
		},
		{
			name: "empty slice",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d want %d (%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d:\n got  %q\n want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// RedactSecrets must never drop or reorder lines — only mutate secret spans.
func TestRedactSecretsPreservesLineCount(t *testing.T) {
	in := []string{"a", "PASSWORD=topsecret", "b", "c"}
	got := RedactSecrets(in)
	if len(got) != len(in) {
		t.Fatalf("line count changed: got %d want %d", len(got), len(in))
	}
	if got[0] != "a" || got[2] != "b" || got[3] != "c" {
		t.Errorf("non-secret lines altered: %v", got)
	}
	if !strings.Contains(got[1], "***") || strings.Contains(got[1], "topsecret") {
		t.Errorf("secret not redacted: %q", got[1])
	}
}
