//go:build unix

package launcher

import (
	"os"
	"syscall"
)

// gracefulSignal asks the server to shut down; the C server handles SIGTERM.
// It is used only without a parent lifeline.
func gracefulSignal(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
