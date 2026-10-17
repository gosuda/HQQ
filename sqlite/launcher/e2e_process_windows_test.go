//go:build windows

package launcher_test

import (
	"fmt"
	"syscall"
)

const (
	processQueryLimitedInformation = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION (not exported by package syscall)
	stillActive                    = 259    // STILL_ACTIVE: the exit code of a process that has not exited
)

// probeProcess returns nil while process pid runs. os.Process.Signal is not
// implemented on Windows, so the probe reads the exit code of the process handle.
// A process whose handle cannot be opened has been reaped and is not running.
func probeProcess(pid int) error {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("process %d: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return fmt.Errorf("process %d: %w", pid, err)
	}
	if code != stillActive {
		return fmt.Errorf("process %d exited with code %d", pid, code)
	}
	return nil
}
