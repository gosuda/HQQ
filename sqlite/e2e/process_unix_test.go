//go:build !windows

package e2e

import (
	"os/exec"
	"syscall"
)

// setProcessGroup prepares cmd for gracefulStop. POSIX needs nothing: SIGTERM
// goes to the process itself, and the server stays in the test's process group
// as it does for a manual run.
func setProcessGroup(*exec.Cmd) {}

// gracefulStop asks the server to shut down gracefully: SIGTERM, which the C
// server handles on its graceful path (exit status 0).
func gracefulStop(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}
