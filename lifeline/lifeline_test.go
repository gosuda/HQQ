package lifeline_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gosuda.org/hqq/lifeline"
)

const roleEnv = "LIFELINE_TEST_ROLE"

// helper returns a command that runs this test binary in role. env is added to
// the environment.
func helper(t *testing.T, role string, env ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(append(os.Environ(), roleEnv+"="+role), env...)
	return cmd
}

// waitFile waits until path exists, or fails the test after d.
func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within %v", path, d)
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// paths returns the ready and mark file names of a test.
func paths(t *testing.T) (ready, mark string) {
	dir := t.TempDir()
	return filepath.Join(dir, "ready"), filepath.Join(dir, "mark")
}

func TestEOFCancelsChild(t *testing.T) {
	ready, mark := paths(t)
	cmd := helper(t, "watch", "LIFELINE_READY="+ready, "LIFELINE_MARK="+mark)
	ll, err := lifeline.Attach(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, ready, 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	if exists(mark) {
		t.Fatal("child canceled while the pipe was still open")
	}
	if err := ll.Close(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, mark, 5*time.Second)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v", err)
	}
}

func TestKeepaliveBytesAreWrittenAndIgnored(t *testing.T) {
	ready, mark := paths(t)
	cmd := helper(t, "watch", "LIFELINE_READY="+ready, "LIFELINE_MARK="+mark)
	ll, err := lifeline.Attach(cmd, lifeline.WithKeepalive(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, ready, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if exists(mark) {
		t.Fatal("keepalive bytes canceled the watch")
	}
	if err := ll.Close(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, mark, 5*time.Second)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v", err)
	}

	// The same keepalive, read by a plain reader: the bytes do arrive.
	count := filepath.Join(t.TempDir(), "count")
	cmd = helper(t, "count", "LIFELINE_MARK="+count)
	ll, err = lifeline.Attach(cmd, lifeline.WithKeepalive(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := ll.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("count child: %v", err)
	}
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 5 {
		t.Fatalf("child read %q keepalive bytes, want at least 5", b)
	}
}

func TestParentKilledCancelsChild(t *testing.T) {
	ready, mark := paths(t)
	// The parent is a helper process: it holds the write end and starts the
	// watch child. Killing it closes its descriptors, as a SIGKILL does.
	holder := helper(t, "holder", "LIFELINE_READY="+ready, "LIFELINE_MARK="+mark)
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer holder.Process.Kill()
	waitFile(t, ready, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if exists(mark) {
		t.Fatal("child canceled while its parent was alive")
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	start := time.Now()
	waitFile(t, mark, 2*time.Second)
	t.Logf("child canceled %v after the parent was killed", time.Since(start).Round(time.Millisecond))
}

func TestTerminalIsNotWatched(t *testing.T) {
	// No pipe: the child's stdin is /dev/null, which is at EOF at once. The
	// role forces the terminal check, so the watch must not start.
	out, err := helper(t, "tty").Output()
	if err != nil {
		t.Fatalf("tty role: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "not-watched" {
		t.Fatalf("tty role printed %q, want not-watched", got)
	}
	out, err = helper(t, "tty-forced").Output()
	if err != nil {
		t.Fatalf("tty-forced role: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "forced" {
		t.Fatalf("tty-forced role printed %q, want forced", got)
	}
}

func TestStopAndParentCancel(t *testing.T) {
	out, err := helper(t, "stop").Output()
	if err != nil {
		t.Fatalf("stop role: %v", err)
	}
	want := "stopped\nparent-canceled\n"
	if !bytes.Equal(out, []byte(want)) {
		t.Fatalf("stop role printed %q, want %q", out, want)
	}
}

func TestAttachErrors(t *testing.T) {
	if _, err := lifeline.Attach(nil); err == nil {
		t.Fatal("Attach(nil) succeeded")
	}

	cmd := helper(t, "tty")
	cmd.Stdin = bytes.NewReader(nil)
	if _, err := lifeline.Attach(cmd); err != lifeline.ErrStdinSet {
		t.Fatalf("Attach with Stdin set: %v, want ErrStdinSet", err)
	}

	cmd = helper(t, "tty")
	if _, err := lifeline.Attach(cmd, lifeline.WithKeepalive(-time.Second)); err == nil {
		t.Fatal("negative keepalive accepted")
	}

	cmd = helper(t, "tty")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := lifeline.Attach(cmd); err != lifeline.ErrStarted {
		t.Fatalf("Attach after Start: %v, want ErrStarted", err)
	}
	_ = cmd.Wait()
}

func TestCloseIsIdempotent(t *testing.T) {
	cmd := helper(t, "tty")
	ll, err := lifeline.Attach(cmd, lifeline.WithKeepalive(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := ll.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ll.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
