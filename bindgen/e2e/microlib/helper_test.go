//go:build unix

package microlib_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// Helper processes. A test that must kill a client or the parent of a server needs a second process
// that it can kill; it re-executes the test binary with envHelper set, which TestMain routes to
// runHelper before any test runs.
const (
	envHelper   = "HQQ_E2E_HELPER"   // "client" or "parent"
	envListener = "HQQ_E2E_LISTENER" // listener of the server the helper talks to
	envBinary   = "HQQ_E2E_BINARY"   // server binary the "parent" helper starts
	envTrace    = "HQQ_E2E_TRACE"    // ML_TRACE file for the server the "parent" helper starts
)

// runHelper is the main of a helper process. It prints "ready" lines on stdout for the test to wait
// for, then waits until its standard input closes or the test kills it.
func runHelper(mode string) int {
	var err error
	switch mode {
	case "client":
		err = helperClient()
	case "parent":
		err = helperParent()
	default:
		err = fmt.Errorf("unknown helper mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		return 1
	}
	return 0
}

// helperClient opens a session, creates two counters with one cursor each (counter 1, counter 2,
// cursor 1 of counter 1, cursor 2 of counter 2, in that order: creation sequence 1 to 4), and waits to be
// killed.
func helperClient() error {
	ctx, cancel := context.WithTimeout(context.Background(), failBound)
	defer cancel()
	c, err := dialWith(ctx, os.Getenv(envListener), rt.Options{Link: fastLink})
	if err != nil {
		return err
	}
	c1, err := c.CounterNew(1)
	if err != nil {
		return err
	}
	c2, err := c.CounterNew(2)
	if err != nil {
		return err
	}
	if _, err = c.CursorOpen(c1, 1); err != nil {
		return err
	}
	if _, err = c.CursorOpen(c2, 1); err != nil {
		return err
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return nil
}

// helperParent starts the server the way a launcher does (with the lifeline) and reports its pid.
func helperParent() error {
	bin := os.Getenv(envBinary)
	hash, _, err := secureexec.HashFile(bin)
	if err != nil {
		return err
	}
	name := os.Getenv(envListener)
	srv, err := launch.Start(context.Background(), launch.Config{
		Binary:   bin,
		SHA256:   hash,
		Listener: name,
		Args:     []string{name},
		Env:      append(os.Environ(), "ML_TRACE="+os.Getenv(envTrace)),
	})
	if err != nil {
		return err
	}
	fmt.Println("pid", srv.PID())
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return srv.Close()
}

// helper is a running helper process.
type helper struct {
	cmd    *exec.Cmd
	pre    []string // output lines printed before "ready"
	exited chan struct{}
}

// startHelper runs the test binary as a helper of the given mode and waits for its "ready" line. The
// helper is killed at the end of the test if it is still running.
func startHelper(t *testing.T, mode string, env ...string) *helper {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), append(env, envHelper+"="+mode)...)
	stderr := newTextBuf()
	cmd.Stderr = stderr
	// The helper waits for end of file on its standard input; the pipe is never written.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the %s helper: %v", mode, err)
	}
	h := &helper{cmd: cmd, exited: make(chan struct{})}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		_ = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-h.exited
	})
	deadline := time.After(failBound)
	for {
		select {
		case l, ok := <-lines:
			switch {
			case !ok:
				t.Fatalf("the %s helper ended before it was ready:\n%s", mode, stderr.String())
			case l == "ready":
				return h
			}
			h.pre = append(h.pre, l)
		case <-deadline:
			t.Fatalf("the %s helper is not ready after %v:\n%s", mode, failBound, stderr.String())
		}
	}
}

// kill ends the helper with SIGKILL and waits until it is gone.
func (h *helper) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9 helper: %v", err)
	}
	select {
	case <-h.exited:
	case <-time.After(failBound):
		t.Fatalf("helper still running %v after SIGKILL", failBound)
	}
}
