package store_test

import (
	"context"
	"testing"

	"github.com/alehatsman/dex/internal/store"
)

// openTestStore opens a fresh store backed by a temp-dir SQLite file and
// registers cleanup. Shared across the store_test package.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
