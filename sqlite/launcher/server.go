package launcher

import (
	"context"

	"gosuda.org/hqq/launch"
)

// Server is a running HQQ SQLite server started by Start. Its Listener is the
// name clients connect to. Done is closed when the process has exited and the
// verified snapshot is released. Stop and Close are safe to call more than once
// and from any goroutine. It is launch.Server, which documents the methods.
type Server = launch.Server

// StartError reports that the server could not be started: verification
// failed (Err wraps secureexec.ErrHashMismatch, secureexec.ErrUnsupported,
// ...), the process could not be created, or it exited before it was ready.
// Output is the end of the server's stdout and stderr. It is launch.StartError.
type StartError = launch.StartError

// ReadyTimeoutError reports that the server did not print its ready line within
// the ReadyTimeout. The process was killed. Output is the end of its output. It
// is launch.ReadyTimeoutError.
type ReadyTimeoutError = launch.ReadyTimeoutError

// Start verifies the server binary, starts it with the listener and database
// policy of cfg and waits until it prints its ready line. ctx bounds only the
// start: the server keeps running until Stop or Close.
//
// The binary is either the one at cfg.Binary with the digest cfg.SHA256, or the
// entry for this platform of a signed manifest (cfg.Manifest). Both are checked
// with secureexec.Verify, which hashes the bytes that will run. A mismatch
// returns a *StartError that wraps secureexec.ErrHashMismatch, and no process is
// started.
//
// Unless cfg.DisableLifeline is set, the server gets --parent-stdin and a
// lifeline: it shuts down gracefully when this process dies, even by SIGKILL.
// The process is started by launch.Start.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	lc, err := cfg.launchConfig()
	if err != nil {
		return nil, err
	}
	return launch.Start(ctx, lc, cfg.ExtraArgs...)
}
