package e2e

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Parent-lifeline tests (--parent-stdin): the server's life follows its
// standard input. The write end of a pipe, held by the test or by a helper
// process, plays the supervisor. Closing it (or killing its holder) must make
// the server shut down gracefully on its own.

// waitExitOK waits up to d for srv to exit and fails the test unless it exited
// with status 0.
func waitExitOK(t *testing.T, srv *server, d time.Duration) {
	t.Helper()
	select {
	case <-srv.exited:
	case <-time.After(d):
		srv.kill()
		t.Fatalf("server %s did not exit within %v after its parent went away: %s", srv.name, d, srv.stderr.String())
	}
	if err := srv.waitErr(); err != nil {
		t.Fatalf("%v; stderr: %s", err, srv.stderr.String())
	}
}

// TestParentStdinEOFShutsDownCleanly closes the supervisor's end of the pipe
// while a transaction is open. The server exits with status 0 within a few
// seconds, and the open transaction is rolled back: a second server on the same
// database sees only the committed row.
func TestParentStdinEOFShutsDownCleanly(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := startServerStdin(bin, name, pr, append([]string{"--allow-dir", dir, "--parent-stdin"}, fastServerFlags...)...)
	_ = pr.Close()
	if err != nil {
		_ = pw.Close()
		t.Fatal(err)
	}
	t.Cleanup(srv.close)

	db := openDB(t, name, "main")
	mustExec(t, db, "CREATE TABLE r (v INTEGER)")
	mustExec(t, db, "INSERT INTO r VALUES (1)")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx, "INSERT INTO r VALUES (2)") // left open: the shutdown must roll it back

	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	waitExitOK(t, srv, 5*time.Second)
	if !strings.Contains(srv.stderr.String(), "parent lifeline") {
		t.Fatalf("no lifeline shutdown message: %s", srv.stderr.String())
	}

	srv2, err := startServer(bin, listenerName(), append([]string{"--allow-dir", dir}, fastServerFlags...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv2.close)
	db2 := openDB(t, srv2.name, "main")
	if n := queryInt(t, db2, "SELECT count(*) FROM r"); n != 1 {
		t.Fatalf("rows after the shutdown: %d, want 1 (the open transaction must be rolled back)", n)
	}
	mustExec(t, db2, "INSERT INTO r VALUES (3)") // no lock may be left behind
}

// TestStdinEOFWithoutFlagIsIgnored checks that, without --parent-stdin, closing
// standard input does not stop the server: it still shuts down on a graceful stop.
func TestStdinEOFWithoutFlagIsIgnored(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := startServerStdin(bin, listenerName(), pr, append([]string{"--allow-dir", dir}, fastServerFlags...)...)
	_ = pr.Close()
	if err != nil {
		_ = pw.Close()
		t.Fatal(err)
	}
	t.Cleanup(srv.close)

	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if !srv.alive() {
		t.Fatalf("server exited on stdin EOF without --parent-stdin: %s", srv.stderr.String())
	}
	if err := srv.term(10 * time.Second); err != nil {
		t.Fatalf("graceful exit on stop: %v; stderr: %s", err, srv.stderr.String())
	}
}

// TestParentStdinHolderSIGKILL kills the process that holds the pipe's write end
// with SIGKILL, so the kernel closes the pipe without any cleanup. The server
// must still be running while the holder lives, and exit with status 0 within a
// few seconds after the kill.
func TestParentStdinHolderSIGKILL(t *testing.T) {
	bin := serverBinary(t)
	dir := t.TempDir()
	name := listenerName()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	// The holder is this test binary re-executed. Its standard input is the
	// write end: the child inherits it on every platform (ExtraFiles is not
	// supported on Windows).
	holder := exec.Command(os.Args[0], "-test.run=^TestHelperPipeHolder$", "-test.count=1")
	holder.Env = append(os.Environ(), "HQQSQLITE_E2E_PIPEHOLDER=1")
	holder.Stdin = pw
	holder.Stderr = os.Stderr
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill() })
	_ = pw.Close() // from now on only the holder keeps the write end

	held := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "holding" {
			err = fmt.Errorf("holder reported %q", line)
		}
		held <- err
	}()
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("pipe holder: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipe holder did not start")
	}

	srv, err := startServerStdin(bin, name, pr, append([]string{"--allow-dir", dir, "--parent-stdin"}, fastServerFlags...)...)
	_ = pr.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.close)

	time.Sleep(300 * time.Millisecond)
	if !srv.alive() {
		t.Fatalf("server exited while the pipe holder was alive: %s", srv.stderr.String())
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	waitExitOK(t, srv, 5*time.Second)
}

// TestHelperPipeHolder is the pipe holder of TestParentStdinHolderSIGKILL: it
// keeps the write end it inherited as standard input until it is killed. It
// runs only when HQQSQLITE_E2E_PIPEHOLDER is set.
func TestHelperPipeHolder(t *testing.T) {
	if os.Getenv("HQQSQLITE_E2E_PIPEHOLDER") == "" {
		t.Skip("helper process of TestParentStdinHolderSIGKILL")
	}
	fmt.Println("holding")
	time.Sleep(time.Hour) // the test kills this process
}
