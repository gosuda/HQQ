//go:build windows

package e2e

import (
	"fmt"
	"os/exec"
	"syscall"
)

// Windows has no SIGTERM: os.Process.Signal is not implemented, and
// os.Process.Kill is TerminateProcess, which runs no cleanup. A graceful stop is
// a CTRL_BREAK_EVENT sent to the server's process group, which the server
// handles on its graceful path (exit status 0). The group exists only because
// setProcessGroup started the server with CREATE_NEW_PROCESS_GROUP, so the event
// does not reach the test process.

const ctrlBreakEvent = 1 // CTRL_BREAK_EVENT

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// setProcessGroup starts cmd in a new process group, the target of gracefulStop.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// gracefulStop sends CTRL_BREAK_EVENT to the process group of cmd. It fails when
// the calling process is not attached to a console.
func gracefulStop(cmd *exec.Cmd) error {
	r, _, err := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(cmd.Process.Pid))
	if r == 0 {
		return fmt.Errorf("graceful stop of pid %d: GenerateConsoleCtrlEvent: %w", cmd.Process.Pid, err)
	}
	return nil
}
