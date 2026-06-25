//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
	"time"
)

// shellWaitDelay bounds how long we wait after the context expires before
// abandoning the pipe-copy goroutines. cmd.Cancel kills the process group,
// but a descendant that ignored SIGTERM and clung to stdout could still hold
// the pipe open; WaitDelay forces cmd.Wait to return regardless.
const shellWaitDelay = 2 * time.Second

// setupShellProcessGroup gives the spawned shell its own process group and
// arranges to SIGKILL the whole group on context cancel — not just the shell
// wrapper. Without this, a `bash -c "sleep 60"` outlives its 1s deadline
// because sleep inherits stdout/stderr and keeps the pipes open until it
// exits naturally, blocking cmd.Wait. With it, the deadline actually aborts
// the work.
func setupShellProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole process group rooted at the shell.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = shellWaitDelay
}
