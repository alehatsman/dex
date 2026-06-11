package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alehatsman/dex/internal/chat"
	"github.com/alehatsman/dex/internal/embed"
)

const testSpec = `---
id: test-feature
status: living
last_verified: abc1234
owners: [test]
covers:
  - "internal/feature/**"
---
# Test Feature

## Intent

Test spec for verification.

## Behavior

- WHEN something happens, dex does something useful.
- IF an error occurs, dex returns an error status.

## Checklist

- [x] Does the main thing
- [x] Handles errors gracefully
- [ ] Pending future work
`

func TestSpecVerifyNoSpec(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	s := newServer(srv.URL, t.TempDir())

	projDir := t.TempDir()
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    "specs/nonexistent.md",
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-spec" {
		t.Errorf("status = %q, want no-spec", out.Status)
	}
}

func TestSpecVerifyNoIndex(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	specPath := filepath.Join(projDir, "spec.md")
	writeFile(t, specPath, testSpec)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    specPath,
		ProjectRoot: projDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no-index" {
		t.Errorf("status = %q, want no-index", out.Status)
	}
	if out.Hint == "" {
		t.Error("expected a hint for no-index")
	}
}

func TestSpecVerifyOkNoJudge(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "feature.go"),
		"package feature\n\nfunc DoMainThing() error { return nil }\nfunc HandleError(err error) string { return err.Error() }\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)

	specPath := filepath.Join(projDir, "spec.md")
	writeFile(t, specPath, testSpec)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    specPath,
		ProjectRoot: root,
		NoJudge:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.SpecID != "test-feature" {
		t.Errorf("spec_id = %q, want test-feature", out.SpecID)
	}

	// Two checked items + one pending.
	if out.PendingCount != 1 {
		t.Errorf("pending_count = %d, want 1", out.PendingCount)
	}
	// Checked items should be unknown (no judge) with cites populated.
	checkedResults := 0
	for _, r := range out.Results {
		if r.Checked {
			checkedResults++
			if r.Status != "unknown" && r.Status != "no-evidence" {
				t.Errorf("item %q: status = %q, want unknown or no-evidence", r.Item, r.Status)
			}
		} else {
			if r.Status != "pending" {
				t.Errorf("unchecked item %q: status = %q, want pending", r.Item, r.Status)
			}
		}
	}
	if checkedResults != 2 {
		t.Errorf("checked item count = %d, want 2", checkedResults)
	}
}

func TestSpecVerifyWithJudge(t *testing.T) {
	embedSrv := fakeEmbed(t, 16)
	defer embedSrv.Close()

	// Fake chat server that always returns pass.
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message":       map[string]string{"role": "assistant", "content": `{"status":"pass","reason":"code clearly implements this"}`},
					"finish_reason": "stop",
				},
			},
			"model": "fake",
		})
	}))
	defer chatSrv.Close()

	cacheDir := t.TempDir()
	projDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "feature.go"),
		"package feature\n\nfunc DoMainThing() error { return nil }\nfunc HandleError(err error) string { return err.Error() }\n")
	root := indexProject(t, projDir, cacheDir, embedSrv.URL)

	specPath := filepath.Join(projDir, "spec.md")
	writeFile(t, specPath, testSpec)

	s := &Server{
		EmbedClient: embed.New(embedSrv.URL, "fake", 16, 5*time.Second),
		ChatClient:  chat.New(chatSrv.URL, "fake", 5*time.Second),
		IndexDir:    cacheDir,
	}
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    specPath,
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Fatalf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
	if out.PassCount == 0 {
		t.Errorf("pass_count = 0, want > 0 (fake chat always returns pass)")
	}
}

func TestSpecVerifyEmbedUnreachable(t *testing.T) {
	embedSrv := fakeEmbed(t, 16)
	defer embedSrv.Close()

	cacheDir := t.TempDir()
	projDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "feature.go"), "package feature\nfunc F() {}\n")
	root := indexProject(t, projDir, cacheDir, embedSrv.URL)

	specPath := filepath.Join(projDir, "spec.md")
	writeFile(t, specPath, testSpec)

	// Point at a dead embedder for the actual query.
	s := &Server{
		EmbedClient: embed.New(closedURL(t), "fake", 16, 200*time.Millisecond),
		IndexDir:    cacheDir,
	}
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    specPath,
		ProjectRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "embedding-service-unreachable" {
		t.Errorf("status = %q, want embedding-service-unreachable", out.Status)
	}
}

func TestParseSpec(t *testing.T) {
	spec := parseSpec([]byte(testSpec))

	if spec.id != "test-feature" {
		t.Errorf("id = %q, want test-feature", spec.id)
	}
	if spec.lastVerified != "abc1234" {
		t.Errorf("last_verified = %q, want abc1234", spec.lastVerified)
	}
	if len(spec.covers) != 1 || spec.covers[0] != "internal/feature/**" {
		t.Errorf("covers = %v, want [internal/feature/**]", spec.covers)
	}
	// Two [x] items + one [ ] item.
	if len(spec.items) != 3 {
		t.Fatalf("items len = %d, want 3; items: %v", len(spec.items), spec.items)
	}
	if !spec.items[0].checked {
		t.Error("item[0] should be checked")
	}
	if !spec.items[1].checked {
		t.Error("item[1] should be checked")
	}
	if spec.items[2].checked {
		t.Error("item[2] should be unchecked (pending)")
	}
}

func TestParseSpecFallbackToBehavior(t *testing.T) {
	specNoCL := `---
id: no-checklist
---
# No Checklist

## Behavior

- WHEN an event arrives, dex processes it.
- IF an error occurs, dex logs it.
- Some non-clause line that should be skipped.
`
	spec := parseSpec([]byte(specNoCL))
	if len(spec.items) != 2 {
		t.Errorf("items len = %d, want 2 (only WHEN/IF clauses)", len(spec.items))
	}
	for _, item := range spec.items {
		if item.checked {
			t.Error("behavior-fallback items should not be checked")
		}
	}
}

func TestDetectDrift(t *testing.T) {
	// Use the actual dex repo — it has git history.
	repoRoot := filepath.Join("..") // internal/mcp/../ = internal/
	// Run from a commit that definitely has ancestors: HEAD~1 to HEAD.
	// If there are no commits touching internal/mcp in that range, just
	// check the function doesn't panic.
	commits, drifted := detectDrift(repoRoot, "HEAD~3", []string{"mcp"})
	// We can't assert on the exact commit count, but the function must not error.
	_ = commits
	_ = drifted
}

func TestSpecVerifyRelativePath(t *testing.T) {
	srv := fakeEmbed(t, 16)
	defer srv.Close()
	cacheDir := t.TempDir()
	projDir := t.TempDir()

	writeFile(t, filepath.Join(projDir, "feature.go"), "package feature\nfunc F() {}\n")
	root := indexProject(t, projDir, cacheDir, srv.URL)

	// Write spec relative to project root.
	writeFile(t, filepath.Join(root, "specs", "test.md"), testSpec)

	s := newServer(srv.URL, cacheDir)
	_, out, err := s.specVerify(context.Background(), nil, SpecVerifyInput{
		SpecPath:    "specs/test.md",
		ProjectRoot: root,
		NoJudge:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok (hint: %q)", out.Status, out.Hint)
	}
}

func TestJudgeSpecItemBadJSON(t *testing.T) {
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return a response where the model includes extra text around the JSON.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message":       map[string]string{"role": "assistant", "content": `Here is my judgment: {"status":"fail","reason":"not implemented"} — done.`},
					"finish_reason": "stop",
				},
			},
			"model": "fake",
		})
	}))
	defer chatSrv.Close()

	s := &Server{ChatClient: chat.New(chatSrv.URL, "fake", 5*time.Second)}
	j := s.judgeSpecItem(context.Background(), "WHEN x happens, dex does y", nil)
	// Should degrade to unknown when hits is empty (judgeSpecItem is called with nil hits).
	// It calls Generate even with nil hits — the result depends on the model output parsing.
	if j.status == "" {
		t.Error("status should never be empty")
	}
	// Should have extracted fail from the embedded JSON.
	if j.status != "fail" {
		t.Errorf("status = %q, want fail (extracted from surrounding text)", j.status)
	}
	if !strings.Contains(j.reason, "not implemented") {
		t.Errorf("reason = %q, want 'not implemented'", j.reason)
	}
}
