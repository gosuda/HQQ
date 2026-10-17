//go:build unix

package launcher

import (
	"os"
	"os/exec"
	"syscall"
)

// gracefulSignal asks the server to shut down; the C server handles SIGTERM.
// It is used only without a parent lifeline.
func gracefulSignal(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// setProcessGroup leaves the server in this process's group, so a terminal's
// SIGINT still reaches it as before. Windows starts it in its own group
// (graceful_windows.go).
func setProcessGroup(*exec.Cmd) {}
