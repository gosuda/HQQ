//go:build !unix

package launcher

import (
	"errors"
	"os"
)

var errNoGracefulSignal = errors.New("launcher: no graceful signal on this platform")

// gracefulSignal has no equivalent on this platform. Stop then kills the
// server when there is no parent lifeline.
func gracefulSignal(*os.Process) error {
	return errNoGracefulSignal
}
