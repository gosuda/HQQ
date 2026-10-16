package interop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const (
	// backstopSlack is added to a scenario's timeout_s to give the driver's
	// per-process deadline. The helper enforces its own --timeout first.
	backstopSlack = 15 * time.Second

	// waitDelay bounds how long Wait waits for the output pipes after the
	// process exits, so a grandchild that keeps a pipe open cannot hang the run.
	waitDelay = 10 * time.Second

	// stderrTailMax is how much of a process's stderr a failure report keeps.
	stderrTailMax = 8 << 10
)

// tailBuffer keeps the last stderrTailMax bytes written to it.
type tailBuffer struct {
	buf   []byte
	total int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*stderrTailMax {
		b.buf = bytes.Clone(b.buf[len(b.buf)-stderrTailMax:])
	}
	return len(p), nil
}

// String returns the tail, prefixed with the number of dropped bytes.
func (b *tailBuffer) String() string {
	kept := b.buf
	if len(kept) > stderrTailMax {
		kept = kept[len(kept)-stderrTailMax:]
	}
	if dropped := b.total - len(kept); dropped > 0 {
		return fmt.Sprintf("[%d bytes truncated]\n%s", dropped, kept)
	}
	return string(kept)
}

// proc is one running helper process.
type proc struct {
	cmd      *exec.Cmd
	ctx      context.Context
	cancel   context.CancelFunc
	stdout   bytes.Buffer
	stderr   tailBuffer
	startErr error
	done     chan struct{} // closed when the process has exited and its output is read
}

// startProc starts bin with args. The process is killed when deadline passes.
// A start failure is reported by outcome, not returned, so that every role of
// a step is still judged.
func startProc(bin string, args []string, deadline time.Duration) *proc {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	p := &proc{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	p.cmd = exec.CommandContext(ctx, bin, args...)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	p.cmd.WaitDelay = waitDelay
	if err := p.cmd.Start(); err != nil {
		p.startErr = err
		close(p.done)
		return p
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	return p
}

// outcome returns what the process did. It must be called after done is closed
// and before cancel, because the backstop test reads the context's deadline.
func (p *proc) outcome() roleOutcome {
	o := roleOutcome{exitCode: -1}
	if p.startErr != nil {
		o.parseErr = "could not start: " + p.startErr.Error()
		return o
	}
	if ps := p.cmd.ProcessState; ps != nil {
		o.exitCode = ps.ExitCode()
	}
	o.stderr = p.stderr.String()
	res, err := parseResult(p.stdout.Bytes())
	if err != nil {
		o.parseErr = err.Error()
	} else {
		o.result = &res
	}
	o.backstop = errors.Is(p.ctx.Err(), context.DeadlineExceeded) && (o.result == nil || o.exitCode == -1)
	return o
}
