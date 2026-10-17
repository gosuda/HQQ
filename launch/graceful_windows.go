//go:build windows

package launch

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Windows has no SIGTERM: os.Process.Signal is not implemented, and Kill is
// TerminateProcess, which runs no cleanup. The server's console control handler
// runs its graceful path (exit status 0) for CTRL_BREAK_EVENT, sent here to the
// server's own process group.

const ctrlBreakEvent = 1 // CTRL_BREAK_EVENT

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// setProcessGroup starts the server in a new process group, which is the target
// of gracefulSignal. Ctrl+C is disabled for that group; Ctrl+BREAK still reaches it.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// gracefulSignal asks the server to shut down gracefully. It fails when this
// process is not attached to a console, and Stop then kills the server.
func gracefulSignal(p *os.Process) error {
	r, _, err := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(p.Pid))
	if r == 0 {
		return fmt.Errorf("launch: CTRL_BREAK to process group %d: %w", p.Pid, err)
	}
	return nil
}
