//go:build !unix

package mcp

import "os/exec"

// setupShellProcessGroup is a no-op on non-Unix platforms. The shell tool
// already only spawns sh/bash, which are unix-shaped; this file exists so the
// package builds on Windows for tooling/IDE purposes.
func setupShellProcessGroup(_ *exec.Cmd) {}
