//go:build unix

package supervisor

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcAttr puts the service in its own process group, so that a Ctrl-C
// sent to hqqd's terminal does not reach it: hqqd stops services itself.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcess delivers a graceful stop signal to the service process.
func signalProcess(p *os.Process, sig syscall.Signal) error {
	return p.Signal(sig)
}

// killProcess kills the service and its process group. The group is only
// signaled when the process itself could be: an unreaped leader keeps its PID,
// and with it the group ID, reserved.
func killProcess(p *os.Process) {
	if err := p.Kill(); err != nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
