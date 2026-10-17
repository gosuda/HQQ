//go:build !windows

package launchtest

import (
	"os"
	"syscall"
)

// ProbeProcess returns nil while process pid runs. Signal 0 checks for the
// process without delivering anything.
func ProbeProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}
