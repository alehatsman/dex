package store_test

import (
	"context"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAgentPostReadRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.AgentAnnounce(ctx, "agent-1", "finder"); err != nil {
		t.Fatalf("announce: %v", err)
	}

	id, err := s.AgentPost(ctx, "agent-1", "bugs", "finding", "found a nil deref in auth.go")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero message id")
	}

	msgs, err := s.AgentRead(ctx, "", "", "", 0, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Category != "finding" {
		t.Errorf("category = %q, want finding", m.Category)
	}
	if m.Topic != "bugs" {
		t.Errorf("topic = %q, want bugs", m.Topic)
	}
}

func TestAgentReadCategoryFilter(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_ = s.AgentAnnounce(ctx, "a1", "r")
	_, _ = s.AgentPost(ctx, "a1", "t", "finding", "auth issue")
	_, _ = s.AgentPost(ctx, "a1", "t", "plan", "refactor auth")
	_, _ = s.AgentPost(ctx, "a1", "t", "error", "panic at startup")

	findings, err := s.AgentRead(ctx, "", "finding", "", 0, 50)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Body != "auth issue" {
		t.Errorf("unexpected body: %q", findings[0].Body)
	}
}

func TestAgentReadFTSQuery(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	_ = s.AgentAnnounce(ctx, "a1", "r")
	_, _ = s.AgentPost(ctx, "a1", "", "", "nil pointer dereference in handler")
	_, _ = s.AgentPost(ctx, "a1", "", "", "authentication token expired")
	_, _ = s.AgentPost(ctx, "a1", "", "", "database connection refused")

	hits, err := s.AgentRead(ctx, "", "", "authentication token", 0, 50)
	if err != nil {
		t.Fatalf("fts read: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 FTS hit, got %d", len(hits))
	}
	if hits[0].Body != "authentication token expired" {
		t.Errorf("unexpected FTS hit: %q", hits[0].Body)
	}
}

func TestAgentConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	_ = s.AgentAnnounce(ctx, "a1", "r")

	const n = 20
	errs := make(chan error, n)
	for i := range n {
		go func(i int) {
			_, err := s.AgentPost(ctx, "a1", "", "note", "msg")
			_ = i
			errs <- err
		}(i)
	}
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("concurrent post error: %v", err)
		}
	}

	msgs, err := s.AgentRead(ctx, "", "", "", 0, 100)
	if err != nil {
		t.Fatalf("read after concurrent posts: %v", err)
	}
	if len(msgs) != n {
		t.Errorf("expected %d messages, got %d", n, len(msgs))
	}
}
