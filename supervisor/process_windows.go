//go:build windows

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

var errNoGracefulSignal = errors.New("supervisor: graceful signals are not available on windows")

// setProcAttr has nothing to set on Windows.
func setProcAttr(*exec.Cmd) {}

// signalProcess always fails on Windows: a graceful stop there needs a
// StopMethod, and the caller kills the process otherwise.
func signalProcess(*os.Process, syscall.Signal) error {
	return errNoGracefulSignal
}

// killProcess kills the service.
func killProcess(p *os.Process) {
	_ = p.Kill()
}
