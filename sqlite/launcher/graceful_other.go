//go:build !unix && !windows

package launcher

import (
	"errors"
	"os"
	"os/exec"
)

var errNoGracefulSignal = errors.New("launcher: no graceful signal on this platform")

// gracefulSignal has no equivalent on this platform. Stop then kills the
// server when there is no parent lifeline.
func gracefulSignal(*os.Process) error {
	return errNoGracefulSignal
}

// setProcessGroup has no process group to set on this platform.
func setProcessGroup(*exec.Cmd) {}
