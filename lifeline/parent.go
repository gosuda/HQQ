package lifeline

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ErrStdinSet is returned by Attach when cmd.Stdin is already set. Attach would
// otherwise replace it silently.
var ErrStdinSet = errors.New("lifeline: cmd.Stdin is already set")

// ErrStarted is returned by Attach when cmd has already been started.
var ErrStarted = errors.New("lifeline: cmd is already started")

// keepaliveByte is the byte written by WithKeepalive. Children discard it.
const keepaliveByte = '\n'

// Option configures Attach.
type Option func(*config)

type config struct {
	keepalive time.Duration
}

// WithKeepalive makes the parent write one byte to the child every d. A child
// built on [Watch] discards the bytes. d must be positive; zero (the default)
// writes nothing.
func WithKeepalive(d time.Duration) Option {
	return func(c *config) { c.keepalive = d }
}

// Lifeline is the parent's end of the pipe. Keep it for as long as the child
// must live, and call Close when the child has exited or when the parent gives
// up on it. Close is idempotent and safe to call from any goroutine.
//
// The parent's copy of the read end is held until Close, because Attach runs
// before cmd.Start and os/exec does not tell the caller when the child has
// started. The read end is never read from, so it does not affect end of file.
type Lifeline struct {
	r, w *os.File
	stop chan struct{} // closed by Close; nil without keepalive

	once     sync.Once
	closeErr error
}

// Attach creates the pipe, sets cmd.Stdin to its read end and returns the
// Lifeline that owns the write end. cmd must not have been started, and its
// Stdin must be nil. Call Close even when cmd.Start fails.
func Attach(cmd *exec.Cmd, opts ...Option) (*Lifeline, error) {
	if cmd == nil {
		return nil, errors.New("lifeline: nil cmd")
	}
	if cmd.Stdin != nil {
		return nil, ErrStdinSet
	}
	if cmd.Process != nil {
		return nil, ErrStarted
	}
	var c config
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	if c.keepalive < 0 {
		return nil, fmt.Errorf("lifeline: negative keepalive interval %v", c.keepalive)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("lifeline: pipe: %w", err)
	}
	l := &Lifeline{r: r, w: w}
	cmd.Stdin = r
	if c.keepalive > 0 {
		l.stop = make(chan struct{})
		go l.keepalive(c.keepalive)
	}
	return l, nil
}

// keepalive writes one byte every d until Close, or until a write fails.
func (l *Lifeline) keepalive(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if _, err := l.w.Write([]byte{keepaliveByte}); err != nil {
				return
			}
		}
	}
}

// Close closes the write end, so the child reads end of file unless another
// holder of the write end remains. It also closes the parent's read end. Close
// does not wait for the child to exit; use cmd.Wait or a process watcher for
// that.
func (l *Lifeline) Close() error {
	l.once.Do(func() {
		if l.stop != nil {
			close(l.stop)
		}
		l.closeErr = l.w.Close()
		if err := l.r.Close(); err != nil && l.closeErr == nil {
			l.closeErr = err
		}
	})
	return l.closeErr
}
