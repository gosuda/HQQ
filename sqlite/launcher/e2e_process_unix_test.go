//go:build !windows

package launcher_test

import (
	"os"
	"syscall"
)

// probeProcess returns nil while process pid runs. Signal 0 checks for the
// process without delivering anything.
func probeProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}
