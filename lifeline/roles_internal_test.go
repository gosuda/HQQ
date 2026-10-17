package lifeline

// The tests re-execute this test binary with LIFELINE_TEST_ROLE set. TestMain
// runs the role instead of the tests. The roles are children (watch, count,
// tty, tty-forced, stop) and one parent (holder) that starts a watch child and
// holds the pipe's write end until it is killed.
//
// This file is package lifeline (not lifeline_test) so that roles can force
// stdinIsTerminal.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const roleEnv = "LIFELINE_TEST_ROLE"

func TestMain(m *testing.M) {
	if role := os.Getenv(roleEnv); role != "" {
		runRole(role)
	}
	os.Exit(m.Run())
}

func runRole(role string) {
	switch role {
	case "watch":
		// Writes LIFELINE_READY once watching, LIFELINE_MARK when canceled.
		ctx, stop := Watch(context.Background())
		defer stop()
		mark(os.Getenv("LIFELINE_READY"))
		<-ctx.Done()
		mark(os.Getenv("LIFELINE_MARK"))
		syscall.Exit(0)
	case "count":
		// Counts the bytes of stdin up to end of file (keepalive check).
		n, _ := io.Copy(io.Discard, os.Stdin)
		mark2(os.Getenv("LIFELINE_MARK"), fmt.Sprint(n))
		syscall.Exit(0)
	case "tty":
		// A terminal is not watched: stdin at EOF must not cancel.
		stdinIsTerminal = func(*os.File) bool { return true }
		ctx, stop := Watch(context.Background())
		defer stop()
		select {
		case <-ctx.Done():
			fmt.Println("watched")
		case <-time.After(300 * time.Millisecond):
			fmt.Println("not-watched")
		}
		syscall.Exit(0)
	case "tty-forced":
		// WatchTerminal watches a terminal anyway.
		stdinIsTerminal = func(*os.File) bool { return true }
		ctx, stop := Watch(context.Background(), WatchTerminal())
		defer stop()
		select {
		case <-ctx.Done():
			fmt.Println("forced")
		case <-time.After(5 * time.Second):
			fmt.Println("timeout")
		}
		syscall.Exit(0)
	case "stop":
		// The stop function and parent cancellation both cancel the context.
		ctx, stop := Watch(context.Background())
		stop()
		<-ctx.Done()
		fmt.Println("stopped")
		parent, cancelParent := context.WithCancel(context.Background())
		ctx2, stop2 := Watch(parent)
		defer stop2()
		cancelParent()
		<-ctx2.Done()
		fmt.Println("parent-canceled")
		syscall.Exit(0)
	case "holder":
		// The parent: starts a watch child on a pipe and holds the write end
		// until it is killed by the test.
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			syscall.Exit(2)
		}
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), roleEnv+"=watch")
		cmd.Stderr = os.Stderr
		ll, err := Attach(cmd)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			syscall.Exit(2)
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			syscall.Exit(2)
		}
		time.Sleep(time.Hour)
		_ = ll.Close()
		syscall.Exit(0)
	default:
		fmt.Fprintln(os.Stderr, "unknown role", role)
		syscall.Exit(2)
	}
}

// mark creates the file at path (no-op when path is empty).
func mark(path string) {
	if path != "" {
		_ = os.WriteFile(path, []byte("x"), 0o600)
	}
}

// mark2 writes content to the file at path.
func mark2(path, content string) {
	if path != "" {
		_ = os.WriteFile(path, []byte(strings.TrimSpace(content)), 0o600)
	}
}
