package launcher

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"time"

	"gosuda.org/hqq/lifeline"
	"gosuda.org/hqq/secureexec"
)

// waitDelay bounds how long Wait keeps copying output after the server exited,
// in case a grandchild still holds its stdout or stderr.
const waitDelay = 2 * time.Second

// Server is a running HQQ SQLite server started by Start. Its Listener is the
// name clients connect to. Done is closed when the process has exited and the
// verified snapshot is released. Stop and Close are safe to call more than once
// and from any goroutine.
type Server struct {
	listener    string
	cmd         *exec.Cmd
	ll          *lifeline.Lifeline // nil without a parent lifeline
	out         *output
	stopTimeout time.Duration

	done chan struct{} // closed after err is set
	err  error         // exit status: nil for status 0; set before done is closed
}

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
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	path, want, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	listener, err := cfg.listenerName()
	if err != nil {
		return nil, err
	}
	args, err := cfg.args(listener)
	if err != nil {
		return nil, err
	}
	readyTimeout, stopTimeout := cfg.timeouts()

	v, err := secureexec.Verify(path, want, cfg.VerifyOptions...)
	if err != nil {
		return nil, &StartError{Path: path, Err: err}
	}
	// The command is not bound to ctx: the server outlives the start.
	cmd := v.Command(context.Background(), args...)
	if cfg.Env != nil {
		cmd.Env = slices.Clone(cfg.Env)
	}
	var ll *lifeline.Lifeline
	if !cfg.DisableLifeline {
		ll, err = lifeline.Attach(cmd)
		if err != nil {
			_ = v.Close()
			return nil, &StartError{Path: path, Err: err}
		}
	}
	out := newOutput(listener, cfg.Stdout, cfg.Stderr)
	cmd.Stdout = outWriter{o: out, stdout: true}
	cmd.Stderr = outWriter{o: out}
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		if ll != nil {
			_ = ll.Close()
		}
		_ = v.Close()
		return nil, &StartError{Path: path, Err: err}
	}

	s := &Server{
		listener:    listener,
		cmd:         cmd,
		ll:          ll,
		out:         out,
		stopTimeout: stopTimeout,
		done:        make(chan struct{}),
	}
	go s.wait(v)

	timer := time.NewTimer(readyTimeout)
	defer timer.Stop()
	select {
	case <-out.ready:
		return s, nil
	case <-s.done:
		cause := errors.New("exited before it was ready")
		if s.err != nil {
			cause = fmt.Errorf("exited before it was ready: %w", s.err)
		}
		return nil, &StartError{Path: path, Err: cause, Output: out.tailString()}
	case <-timer.C:
		s.kill()
		return nil, &ReadyTimeoutError{Listener: listener, Timeout: readyTimeout, Output: out.tailString()}
	case <-ctx.Done():
		s.kill()
		return nil, &StartError{Path: path, Err: fmt.Errorf("waiting for ready: %w", ctx.Err()), Output: out.tailString()}
	}
}

// wait reaps the process, releases what it holds and then closes done.
func (s *Server) wait(v *secureexec.Verified) {
	err := s.cmd.Wait()
	s.out.flush()
	if s.ll != nil {
		_ = s.ll.Close()
	}
	if cerr := v.Close(); err == nil {
		err = cerr
	}
	s.err = err
	close(s.done)
}

// kill kills the process and waits until it is reaped.
func (s *Server) kill() {
	_ = s.cmd.Process.Kill()
	<-s.done
}

// Listener returns the HQQ listener name the server serves.
func (s *Server) Listener() string { return s.listener }

// PID returns the process id of the server.
func (s *Server) PID() int { return s.cmd.Process.Pid }

// Done is closed once the server has exited.
func (s *Server) Done() <-chan struct{} { return s.done }

// Err returns the exit status error once Done is closed: nil when the server
// exited with status 0. It returns nil while the server runs.
func (s *Server) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Stop shuts the server down and returns its exit status error (nil for status
// 0). With a parent lifeline, Stop closes the pipe, which the server reads as
// end of file, and it waits for the graceful shutdown. Without one it sends
// SIGTERM where the platform has it. If the server has not exited after
// StopTimeout, or ctx ends first, Stop kills it and returns an error; ErrStopTimeout
// matches the first case.
func (s *Server) Stop(ctx context.Context) error {
	if s.ll != nil {
		_ = s.ll.Close()
	} else {
		select {
		case <-s.done:
		default:
			if err := gracefulSignal(s.cmd.Process); err != nil {
				s.kill()
			}
		}
	}
	timer := time.NewTimer(s.stopTimeout)
	defer timer.Stop()
	select {
	case <-s.done:
		return s.err
	case <-timer.C:
		s.kill()
		return fmt.Errorf("%w (after %v; killed, exit: %v)", ErrStopTimeout, s.stopTimeout, s.err)
	case <-ctx.Done():
		s.kill()
		return fmt.Errorf("launcher: stop %s: %w (killed, exit: %v)", s.listener, ctx.Err(), s.err)
	}
}

// Close is Stop with no caller deadline: it returns after StopTimeout at the
// latest. It is idempotent.
func (s *Server) Close() error {
	return s.Stop(context.Background())
}
