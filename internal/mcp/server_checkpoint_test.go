package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckpointHandler exercises snapshot/log/diff through the MCP handler
// against a temp project, asserting the shadow lives in the cache dir and the
// project itself is never turned into a git repo by dex.
func TestCheckpointHandler(t *testing.T) {
	projDir := t.TempDir()
	cacheDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "a.txt"), "hello\n")

	s := &Server{IndexDir: cacheDir}
	ctx := context.Background()

	// snapshot
	_, snap, err := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "snapshot", Message: "first"})
	if err != nil || snap.Status != "ok" {
		t.Fatalf("snapshot: status=%q hint=%q err=%v", snap.Status, snap.Hint, err)
	}
	if !snap.Created || snap.SHA == "" || snap.FilesChanged == 0 {
		t.Fatalf("expected a real checkpoint, got %+v", snap)
	}
	// dex must NOT have created a .git in the user's project.
	if _, err := os.Stat(filepath.Join(projDir, ".git")); err == nil {
		t.Fatal("dex created a .git in the user's project — must use the shadow only")
	}

	// idempotent snapshot — no change.
	_, snap2, _ := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "snapshot"})
	if snap2.Created {
		t.Errorf("unchanged tree should not create a checkpoint, got %+v", snap2)
	}

	// change + snapshot again.
	writeFile(t, filepath.Join(projDir, "b.txt"), "world\n")
	_, snap3, _ := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "snapshot", Message: "added b"})
	if !snap3.Created {
		t.Fatalf("expected a checkpoint after a change, got %+v", snap3)
	}

	// log
	_, lg, err := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "log"})
	if err != nil || lg.Status != "ok" {
		t.Fatalf("log: status=%q err=%v", lg.Status, err)
	}
	if len(lg.Commits) != 2 || lg.Commits[0].Message != "added b" {
		t.Fatalf("unexpected log: %+v", lg.Commits)
	}

	// diff
	_, df, err := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "diff"})
	if err != nil || df.Status != "ok" {
		t.Fatalf("diff: status=%q err=%v", df.Status, err)
	}
	if !strings.Contains(df.Diff, "b.txt") {
		t.Errorf("diff should mention b.txt:\n%s", df.Diff)
	}

	// unknown action
	_, bad, _ := s.checkpoint(ctx, nil, CheckpointInput{ProjectRoot: projDir, Action: "frobnicate"})
	if bad.Status != "error" {
		t.Errorf("unknown action should error, got %+v", bad)
	}
}

func TestCheckpointLogEmpty(t *testing.T) {
	projDir := t.TempDir()
	s := &Server{IndexDir: t.TempDir()}
	_, lg, err := s.checkpoint(context.Background(), nil, CheckpointInput{ProjectRoot: projDir, Action: "log"})
	if err != nil || lg.Status != "ok" {
		t.Fatalf("log: status=%q err=%v", lg.Status, err)
	}
	if len(lg.Commits) != 0 {
		t.Errorf("expected no checkpoints, got %d", len(lg.Commits))
	}
	if !strings.Contains(lg.Hint, "no checkpoints") {
		t.Errorf("expected a 'no checkpoints' hint, got %q", lg.Hint)
	}
}
