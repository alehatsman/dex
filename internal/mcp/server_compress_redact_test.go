package mcp

import (
	"strings"
	"testing"
)

// CompressText is the single chokepoint for shell-output compression. #461
// requires secret redaction there to be uniform across every command type —
// not only the env-filter path. These cases exercise each leak vector named in
// the issue and assert the secret value never survives into the returned text.
func TestCompressTextRedactsSecrets(t *testing.T) {
	ghToken := "ghp_" + strings.Repeat("a", 36)

	cases := []struct {
		name   string
		cmd    string
		output string
		leak   string // must NOT appear in compressed output
		keep   string // must still appear (context preserved)
	}{
		{
			name: "kubectl secret yaml base64 data",
			cmd:  "kubectl get secret db -o yaml",
			output: strings.Join([]string{
				"apiVersion: v1",
				"kind: Secret",
				"metadata:",
				"  name: db",
				"data:",
				"  password: c3VwZXJzZWNyZXQ=",
				"  username: YWRtaW4=",
				"type: Opaque",
			}, "\n"),
			leak: "c3VwZXJzZWNyZXQ=",
			keep: "kind: Secret",
		},
		{
			name: "sql row with password column",
			cmd:  "psql -c 'select * from users'",
			output: strings.Join([]string{
				" id | username |              password",
				"----+----------+----------------------------",
				"  1 | alice    | " + ghToken,
				"  2 | bob      | " + ghToken,
				"(2 rows)",
			}, "\n"),
			leak: ghToken,
			keep: "alice",
		},
		{
			name: "auth flow device code early return",
			cmd:  "gh auth login",
			output: strings.Join([]string{
				"First copy your one-time code: " + ghToken,
				"Open this page in your browser: https://github.com/login/device",
				"user_code: " + ghToken,
				"waiting for authentication...",
			}, "\n"),
			leak: ghToken,
			keep: "browser",
		},
		{
			name: "credential url in output",
			cmd:  "cat config",
			output: strings.Join([]string{
				"loading config from disk",
				"DATABASE_URL=postgres://admin:s3cr3tpw@db.internal:5432/app",
				"connection established",
				"ready to serve requests",
				"listening on :8080",
			}, "\n"),
			leak: "s3cr3tpw",
			keep: "db.internal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compressed, _, _ := CompressText(tc.output, tc.cmd, 0)
			if strings.Contains(compressed, tc.leak) {
				t.Errorf("secret leaked into compressed output:\nleak=%q\ngot=\n%s", tc.leak, compressed)
			}
			if tc.keep != "" && !strings.Contains(compressed, tc.keep) {
				t.Errorf("expected context %q preserved, got:\n%s", tc.keep, compressed)
			}
			if !strings.Contains(compressed, "***") {
				t.Errorf("expected redaction marker '***' in output, got:\n%s", compressed)
			}
		})
	}
}
